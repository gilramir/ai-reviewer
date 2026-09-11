#!/usr/bin/env python3

"""Photograph a page of the review UI by driving a real browser.

The front end is a Gren SPA, and a change to a panel compiles clean while still
looking wrong: a row past the bottom of the window, a command box that wraps
into nonsense, a note that reads as part of the thing above it. `make check`
cannot see any of that. This can, and it costs one command.

It drives whatever Chromium-family browser is installed -- Edge, Chromium or
Chrome -- over the DevTools protocol, so there is nothing to install but the
`websockets` module. The browser is launched headless against a scratch profile
and killed afterwards, so it leaves nothing behind and disturbs no window you
have open.

Point it at a running daemon:

    ai-reviewer serve --root ./docs --no-auth --listen 127.0.0.1:8099 &
    tools/screenshot.py http://127.0.0.1:8099/ panel.png \\
        --click .settings-toggle --scroll-to .setting:nth-of-type(8)

--click and --eval run in the page, in the order given, which is how anything
behind a button gets opened before the shutter. --size is the window the page
believes it is in: the bugs worth catching are the ones that only appear on a
laptop screen, so photograph the small window as well as your own.
"""

import argparse
import asyncio
import base64
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

try:
    import websockets
except ImportError:
    sys.exit("this needs the websockets module: pip install websockets")


# The browsers that speak the DevTools protocol, in the order they are looked
# for. Any of them draws the same page; the first one installed wins.
BROWSERS = [
    "microsoft-edge",
    "microsoft-edge-stable",
    "chromium",
    "chromium-browser",
    "google-chrome",
    "google-chrome-stable",
]


def find_browser(named):
    if named:
        found = shutil.which(named) or (named if os.path.exists(named) else None)
        if not found:
            sys.exit(f"no browser at {named}")
        return found

    for name in BROWSERS:
        found = shutil.which(name)
        if found:
            return found
    sys.exit("no Chromium-family browser found; pass --browser with a path")


def launch(browser, profile):
    """Start the browser headless and return it with the port it is listening on.

    The port is asked for as 0 and read back out of the profile rather than
    picked here, because a fixed number is a race with anything else on the
    machine -- including a previous run of this script that has not finished
    dying yet.
    """
    proc = subprocess.Popen(
        [
            browser,
            "--headless=new",
            "--remote-debugging-port=0",
            f"--user-data-dir={profile}",
            "--no-first-run",
            "--no-default-browser-check",
            "--disable-gpu",
            "about:blank",
        ],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )

    port_file = os.path.join(profile, "DevToolsActivePort")
    deadline = time.time() + 30
    while time.time() < deadline:
        if proc.poll() is not None:
            sys.exit(f"{browser} exited before it was listening")
        try:
            with open(port_file) as f:
                return proc, int(f.readline().strip())
        except (FileNotFoundError, ValueError):
            time.sleep(0.1)

    proc.kill()
    sys.exit(f"{browser} never reported a debugging port")


def page_target(port):
    """The debugger URL of the tab the browser opened for itself."""
    deadline = time.time() + 30
    while time.time() < deadline:
        try:
            raw = urllib.request.urlopen(f"http://127.0.0.1:{port}/json/list").read()
            pages = [t for t in json.loads(raw) if t["type"] == "page"]
            if pages:
                return pages[0]["webSocketDebuggerUrl"]
        except (urllib.error.URLError, ConnectionError):
            pass
        time.sleep(0.1)
    sys.exit("the browser never offered a page to drive")


class Session:
    """One CDP connection, with the request numbering it insists on."""

    def __init__(self, ws):
        self.ws = ws
        self.id = 0

    async def call(self, method, **params):
        self.id += 1
        await self.ws.send(json.dumps({"id": self.id, "method": method, "params": params}))
        while True:
            message = json.loads(await self.ws.recv())
            if message.get("id") != self.id:
                continue  # An event, or an answer to something already done with.
            if "error" in message:
                raise RuntimeError(f"{method}: {message['error']}")
            return message.get("result", {})

    async def evaluate(self, expression):
        result = await self.call(
            "Runtime.evaluate", expression=expression, returnByValue=True, awaitPromise=True
        )
        if "exceptionDetails" in result:
            raise RuntimeError(f"in the page: {result['exceptionDetails'].get('text')}")
        return result.get("result", {}).get("value")


async def shoot(args, debugger):
    async with websockets.connect(debugger, max_size=None) as ws:
        page = Session(ws)

        await page.call("Page.enable")
        await page.call(
            "Emulation.setDeviceMetricsOverride",
            width=args.width,
            height=args.height,
            deviceScaleFactor=args.scale,
            mobile=False,
        )
        opened = await page.call("Page.navigate", url=args.url)
        # A daemon that is not running does not fail the navigation, it fills
        # the window with the browser's own error page -- which photographs
        # perfectly well and says nothing about the change being looked at.
        if opened.get("errorText"):
            raise RuntimeError(f"{args.url}: {opened['errorText']}; is the daemon running?")

        # The SPA is served in one request and then fills itself in over a
        # WebSocket, so the load event is the start of the interesting part
        # rather than the end of it. Hence a settle rather than a wait.
        await asyncio.sleep(args.settle)

        for step in args.steps:
            kind, value = step
            if kind == "click":
                clicked = await page.evaluate(
                    f"(() => {{ const el = document.querySelector({json.dumps(value)});"
                    f" if (!el) return false; el.click(); return true; }})()"
                )
                if not clicked:
                    raise RuntimeError(f"nothing matches {value} to click")
            else:
                await page.evaluate(value)
            await asyncio.sleep(args.step_settle)

        if args.scroll_to:
            found = await page.evaluate(
                f"(() => {{ const el = document.querySelector({json.dumps(args.scroll_to)});"
                f" if (!el) return false;"
                f" el.scrollIntoView({{block: 'center'}}); return true; }})()"
            )
            if not found:
                raise RuntimeError(f"nothing matches {args.scroll_to} to scroll to")
            await asyncio.sleep(args.step_settle)

        shot = await page.call("Page.captureScreenshot", format="png", captureBeyondViewport=False)
        with open(args.out, "wb") as f:
            f.write(base64.b64decode(shot["data"]))


class Step(argparse.Action):
    """Collects --click and --eval into one list, in the order they were given."""

    def __call__(self, parser, namespace, value, option_string=None):
        kind = "click" if option_string == "--click" else "eval"
        namespace.steps.append((kind, value))


def main():
    parser = argparse.ArgumentParser(
        description="Screenshot a page of the review UI, driven over CDP.",
        epilog="Needs a running daemon: ai-reviewer serve --no-auth --listen 127.0.0.1:8099",
    )
    parser.add_argument("url", help="what to open, e.g. http://127.0.0.1:8099/")
    parser.add_argument("out", help="where to write the PNG")
    parser.add_argument(
        "--size",
        default="1280x800",
        help="the window the page thinks it is in (default 1280x800)",
    )
    parser.add_argument(
        "--scale", type=float, default=2.0, help="device pixel ratio (default 2)"
    )
    parser.add_argument(
        "--click", action=Step, metavar="SELECTOR", help="click this before the shot; repeatable"
    )
    parser.add_argument(
        "--eval", action=Step, metavar="JS", help="run this in the page; repeatable"
    )
    parser.add_argument("--scroll-to", metavar="SELECTOR", help="bring this into view last")
    parser.add_argument(
        "--settle", type=float, default=3.0, help="seconds to let the app draw itself (default 3)"
    )
    parser.add_argument(
        "--step-settle", type=float, default=0.6, help="seconds after each step (default 0.6)"
    )
    parser.add_argument("--browser", help="path to a Chromium-family browser")
    parser.set_defaults(steps=[])
    args = parser.parse_args()

    try:
        width, height = (int(n) for n in args.size.lower().split("x"))
    except ValueError:
        sys.exit(f"--size wants WIDTHxHEIGHT, not {args.size!r}")
    args.width, args.height = width, height

    browser = find_browser(args.browser)
    profile = tempfile.mkdtemp(prefix="ai-reviewer-shot-")
    proc, port = launch(browser, profile)
    failure = None
    try:
        asyncio.run(shoot(args, page_target(port)))
    except RuntimeError as e:
        # Nothing here is a bug in this script worth a traceback: the selector
        # was wrong, or the page was not the one expected.
        failure = str(e)
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        shutil.rmtree(profile, ignore_errors=True)

    if failure:
        sys.exit(failure)
    print(f"wrote {args.out} at {args.width}x{args.height}")


if __name__ == "__main__":
    main()
