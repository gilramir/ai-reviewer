#!/usr/bin/env python3
"""Two presentation slides on driving Claude Code from a program.

    python3 tools/slides.py

Rewrites docs/slides-1-one-shot.png, docs/slides-2-pipes.png and
docs/slides-3-chat-api.png. Needs Pillow
and the Ubuntu font family; everything is drawn at 3x and downsampled, because
PIL does not antialias lines.
"""
import math
import os
import re
from PIL import Image, ImageDraw, ImageFont

OUT = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "docs")

# --- drawing kit: supersampled boxes, arrows and text on PIL ----------------

S = 3                                   # supersample factor
UB = "/usr/share/fonts/truetype/ubuntu/"

INK   = (22, 24, 29)
SUB   = (112, 120, 132)
LINE  = (201, 207, 215)
WASH  = (246, 247, 249)
WHITE = (255, 255, 255)

CLAUDE, CLAUDE_BG, CLAUDE_SOFT = (204, 111, 32), (253, 242, 230), (247, 226, 202)
PROG,   PROG_BG,   PROG_SOFT   = (43, 104, 176), (234, 242, 251), (203, 223, 244)
GIT,    GIT_BG                 = (39, 130, 100), (232, 246, 240)
SLATE,  SLATE_BG               = (71, 81, 95),   (240, 242, 245)
REMOTE, REMOTE_BG              = (39, 130, 100), (232, 246, 240)

_fc = {}
def F(kind, size):
    k = (kind, size)
    if k not in _fc:
        p = {'r': 'Ubuntu-R.ttf', 'm': 'Ubuntu-M.ttf', 'b': 'Ubuntu-B.ttf',
             'i': 'Ubuntu-RI.ttf', 'mi': 'Ubuntu-MI.ttf',
             'mono': 'UbuntuMono-R.ttf', 'monob': 'UbuntuMono-B.ttf'}[kind]
        _fc[k] = ImageFont.truetype(UB + p, int(round(size * S)))
    return _fc[k]


class Canvas:
    def __init__(self, w, h, bg=WHITE):
        self.w, self.h = w, h
        self.img = Image.new("RGB", (w * S, h * S), bg)
        self.d = ImageDraw.Draw(self.img)

    # --- shapes -----------------------------------------------------------
    def rrect(self, x, y, w, h, r=10, fill=None, stroke=None, sw=2):
        xy = [x * S, y * S, (x + w) * S, (y + h) * S]
        self.d.rounded_rectangle(xy, radius=int(r * S), fill=fill,
                                 outline=stroke, width=max(1, int(round(sw * S))))

    def rect(self, x, y, w, h, fill=None, stroke=None, sw=2):
        self.d.rectangle([x * S, y * S, (x + w) * S, (y + h) * S], fill=fill,
                         outline=stroke, width=max(1, int(round(sw * S))))

    def circle(self, cx, cy, r, fill=None, stroke=None, sw=2):
        self.d.ellipse([(cx - r) * S, (cy - r) * S, (cx + r) * S, (cy + r) * S],
                       fill=fill, outline=stroke, width=max(1, int(round(sw * S))))

    def hline(self, x1, x2, y, color=LINE, sw=1.5, dash=None):
        self._seg((x1 * S, y * S), (x2 * S, y * S), color, max(1, int(round(sw * S))), dash)

    # --- text -------------------------------------------------------------
    def text(self, x, y, s, kind='r', size=18, fill=INK, anchor='la', spacing=1.35):
        f = F(kind, size)
        if "\n" in s:
            self.d.multiline_text((x * S, y * S), s, font=f, fill=fill, anchor=anchor,
                                  spacing=int(size * S * (spacing - 1)))
        else:
            self.d.text((x * S, y * S), s, font=f, fill=fill, anchor=anchor)

    def tw(self, s, kind='r', size=18):
        return self.d.textlength(s, font=F(kind, size)) / S

    def chip(self, x, y, s, kind='m', size=13, fg=INK, bg=WASH, pad=7, h=22, r=11, stroke=None):
        w = self.tw(s, kind, size) + pad * 2
        self.rrect(x, y, w, h, r=r, fill=bg, stroke=stroke, sw=1.2)
        self.text(x + w / 2, y + h / 2 + 1, s, kind, size, fg, anchor='mm')
        return w

    def badge(self, cx, cy, n, color=INK, r=13):
        self.circle(cx, cy, r, fill=color)
        self.text(cx, cy + 1, str(n), 'b', 14, WHITE, anchor='mm')

    # --- arrows -----------------------------------------------------------
    def _seg(self, a, b, color, w, dash):
        if not dash:
            self.d.line([a, b], fill=color, width=w)
            return
        on, off = dash[0] * S, dash[1] * S
        dx, dy = b[0] - a[0], b[1] - a[1]
        ln = math.hypot(dx, dy) or 1
        ux, uy = dx / ln, dy / ln
        t = 0.0
        while t < ln:
            e = min(t + on, ln)
            self.d.line([(a[0] + ux * t, a[1] + uy * t), (a[0] + ux * e, a[1] + uy * e)],
                        fill=color, width=w)
            t = e + off

    def _head(self, a, b, color, size):
        dx, dy = b[0] - a[0], b[1] - a[1]
        ln = math.hypot(dx, dy) or 1
        ux, uy = dx / ln, dy / ln
        px, py = -uy, ux
        L, W = size * S, size * S * 0.52
        p1 = (b[0] - ux * L + px * W, b[1] - uy * L + py * W)
        p2 = (b[0] - ux * L - px * W, b[1] - uy * L - py * W)
        self.d.polygon([b, p1, p2], fill=color)

    def arrow(self, pts, color=INK, width=2.4, head=11, dash=None, both=False):
        p = [(x * S, y * S) for x, y in pts]
        w = max(1, int(round(width * S)))
        # pull the line back so it does not poke through the head
        def pull(fr, to, d):
            dx, dy = to[0] - fr[0], to[1] - fr[1]
            ln = math.hypot(dx, dy) or 1
            return (to[0] - dx / ln * d, to[1] - dy / ln * d)
        q = list(p)
        q[-1] = pull(p[-2], p[-1], head * S * 0.85)
        if both:
            q[0] = pull(p[1], p[0], head * S * 0.85)
        for i in range(len(q) - 1):
            self._seg(q[i], q[i + 1], color, w, dash)
        if len(p) > 2:                  # round the elbows
            for j in p[1:-1]:
                self.d.ellipse([j[0] - w / 2, j[1] - w / 2, j[0] + w / 2, j[1] + w / 2], fill=color)
        self._head(p[-2], p[-1], color, head)
        if both:
            self._head(p[1], p[0], color, head)

    def save(self, path):
        self.img.resize((self.w, self.h), Image.LANCZOS).save(path, optimize=True)
        return path




def frame(c, title, kicker, foot=None, footbold=None):
    """Shared chrome: rule, title, kicker, optional footer band."""
    c.rect(0, 0, c.w, 7, fill=CLAUDE)
    c.text(60, 46, title, 'b', 31, INK)
    c.text(60, 92, kicker, 'r', 19, SUB)
    if foot:
        c.rrect(60, c.h - 112, c.w - 120, 72, r=10, fill=WASH)
        rich(c, 86, c.h - 92, foot, c.w - 200, 17, (60, 66, 76))
        if footbold:
            c.text(86, c.h - 63, footbold, 'm', 17, INK)


def side_panel(c, x, y, w, h, title):
    c.rrect(x, y, w, h, r=14, fill=PROG_BG, stroke=PROG_SOFT, sw=2)
    c.text(x + 24, y + 26, title, 'm', 19, PROG)


def bullets(c, x, y, items, width, size=15, gap=10):
    """Bullets with hanging indent; a run in `backticks` is set in mono."""
    for item in items:
        c.circle(x + 4, y + size * 0.62, 2.6, fill=SUB)
        y = rich(c, x + 18, y, item, width - 18, size) + gap
    return y


def rich(c, x, y, s, width, size, color=INK):
    """Word-wrap s into width, setting `backticked` runs in mono. Returns the y
    below the last line."""
    words = []                          # (word, kind, space before it)
    gap = False                         # did the text so far end in a space?
    for i, part in enumerate(s.split('`')):
        kind = 'mono' if i % 2 else 'r'
        # a code run wraps as a unit: a command broken mid-pipe misreads
        for j, wd in enumerate([part] if kind == 'mono' else part.split(' ')):
            if wd:
                words.append((wd, kind, j > 0 or gap))
        gap = part.endswith(' ')
    lh = size * 1.45
    cx = x
    sp = c.tw(' ', 'r', size)
    for n, (wd, kind, spaced) in enumerate(words):
        k = kind if kind == 'r' else 'monob'
        sz = size if kind == 'r' else size + 1
        ww = c.tw(wd, k, sz)
        if n and spaced:
            cx += sp
        if cx > x and cx + ww > x + width:
            cx, y = x, y + lh
        c.text(cx, y, wd, k, sz, color if kind == 'r' else CLAUDE)
        cx += ww
    return y + lh


# --- slide 1 -------------------------------------------------------------------

KEY, STR, LIT, PUNCT = SLATE, (168, 84, 14), PROG, SUB

PY_TOKEN = re.compile(r'"(?:[^"\\]|\\.)*"|\w+|[^"\w]+')
PY_KEYWORDS = {"import", "with", "as", "if", "True", "False", "None"}


def py_line(c, x, y, s, size=14):
    """One line of Python, with keywords and strings picked out."""
    for m in PY_TOKEN.finditer(s):
        tok = m.group(0)
        if tok.startswith('"'):
            kind, color = 'mono', STR
        elif tok in PY_KEYWORDS:
            kind, color = 'monob', PROG
        else:
            kind, color = 'mono', INK
        c.text(x, y, tok, kind, size, color)
        x += c.tw(tok, kind, size)


def slide1():
    c = Canvas(1600, 900)
    frame(c, "Claude as a command: one question, one process",
          "A program runs claude -p, reads stdout, and the process exits. The next call starts from nothing.",
          "To carry a conversation over, pass `--resume <session_id>` from the last answer: a new process, the old context.",
          "Fine for a commit message or a yes/no. A program that talks back and forth wants the pipe on the next slide.")

    # --- the program -------------------------------------------------------
    TOP, BOT = 150, 548
    c.rrect(60, TOP, 220, BOT - TOP, r=14, fill=PROG_BG, stroke=PROG, sw=2.2)
    c.text(170, 186, "your program", 'b', 20, INK, anchor='mm')
    c.text(170, 212, "a script, a CI job,", 'r', 14, SUB, anchor='mm')
    c.text(170, 232, "a git hook", 'r', 14, SUB, anchor='mm')

    lanes = [
        (280, 'git diff | claude -p "Write a commit message"', "Add a retry to the fetcher"),
        (470, 'claude -p "Is this an error? yes or no" < line.log', "yes"),
    ]
    PX, BX, BW, BH = 280, 800, 270, 110
    for i, (cy, cmd, out) in enumerate(lanes):
        c.badge(PX + 30, cy - 52, i + 1, PROG, r=12)
        # request: the command line
        c.text(PX + 52, cy - 52, cmd, 'monob', 14.5, INK, anchor='lm')
        c.arrow([(PX, cy - 22), (BX, cy - 22)], PROG, 2.6)
        # response
        c.arrow([(BX, cy + 18), (PX, cy + 18)], CLAUDE, 2.6)
        c.text(PX + 24, cy + 30, "text on stdout", 'm', 13, CLAUDE, anchor='lt')
        c.text(PX + 24, cy + 52, out, 'mono', 14, INK, anchor='lt')

        # the short-lived process
        y = cy - BH / 2
        c.rrect(BX, y, BW, BH, r=12, fill=CLAUDE_BG, stroke=CLAUDE, sw=2.2)
        c.text(BX + 22, y + 30, "claude -p", 'monob', 19, INK, anchor='lm')
        c.text(BX + 22, y + 58, "reads files, thinks, answers", 'r', 14, SUB, anchor='lm')
        c.text(BX + 22, y + 80, "process %d of 2" % (i + 1), 'r', 14, SUB, anchor='lm')
        c.chip(BX + BW - 78, y + 18, "exits", 'm', 13, WHITE, CLAUDE, pad=10, h=24, r=12)

        if i == 0:
            sy = cy + 92
            c.hline(PX + 20, BX + BW, sy, LINE, 1.5, dash=(6, 5))
            lbl = "new process, empty context"
            w = c.tw(lbl, 'm', 13) + 22
            c.chip(BX + BW - w, sy - 12, lbl, 'm', 13, SUB, WHITE, pad=11, h=24, r=12, stroke=LINE)

    # --- what -p does ----------------------------------------------------------
    X, W = 1110, 430
    side_panel(c, X, TOP, W, BOT - TOP, "What -p changes")
    y = TOP + 66
    c.text(X + 24, y, 'claude -p "fix the test"', 'monob', 18, INK)
    rich(c, X + 24, y + 30, "`-p` is short for `--print`", W - 48, 14, SUB)
    bullets(c, X + 26, y + 60, [
        "Prints the response to stdout and exits",
        "Takes the prompt from stdin too: `git diff | claude -p \"...\"`",
        "Shows no dialogs, not even the workspace trust check, so grant tools up front: `--permission-mode`, `--tools`",
        "`--output-format` `text`, `json` or `stream-json`; that flag, `--input-format` and `--max-budget-usd` only work with `-p`",
    ], W - 52, size=14.5, gap=8)

    # --- example 2, from Python ------------------------------------------------
    CY0 = 570
    c.rrect(60, CY0, 1480, 204, r=12, fill=WASH, stroke=LINE, sw=1.5)
    c.badge(90, CY0 + 28, 2, PROG, r=12)
    c.text(112, CY0 + 28, "the same call, from Python", 'b', 17, INK, anchor='lm')
    code = [
        'import subprocess',
        'with open("line.log") as log:',
        '    answer = subprocess.run(',
        '        ["claude", "-p", "Is this an error? yes or no"],',
        '        stdin=log, capture_output=True, text=True, check=True,',
        '    ).stdout.strip()',
        'if answer == "yes": ...',
    ]
    for i, ln in enumerate(code):
        py_line(c, 90, CY0 + 56 + i * 20, ln, 14)

    NX = 690
    c.rect(NX - 30, CY0 + 48, 1.5, 140, fill=LINE, sw=0)
    for i, note in enumerate([
        "An argument list, not a shell string: no quoting to get wrong",
        "`stdin=log` is the `< line.log`",
        "`capture_output`: the answer is whatever lands on stdout",
        "`check=True`: a failed run raises instead of answering \"\"",
        "`.strip()`: the reply ends in a newline",
    ]):
        c.circle(NX + 4, CY0 + 63 + i * 29, 2.6, fill=SUB)
        rich(c, NX + 18, CY0 + 54 + i * 29, note, 820, 15)

    c.save(os.path.join(OUT, "slides-1-one-shot.png"))


# --- slide 2 -------------------------------------------------------------------

JSON_TOKEN = re.compile(r'"(?:[^"\\]|\\.)*"(\s*:)?|"(?:[^"\\]|\\.)*$|[-0-9.]+|true|false|null|…|[^"\s]+|\s+')


def json_line(c, x, y, s, size=13.5):
    """One line of JSON with keys, strings and literals told apart by colour.
    A line opening with spaces then a non-JSON character continues a string
    wrapped from the line above, and is set as string throughout."""
    body = s.lstrip(' ')
    if body and body[0] not in '{}[]"':
        x += c.tw(s[:len(s) - len(body)], 'mono', size)
        c.text(x, y, body, 'mono', size, STR)
        return
    for m in JSON_TOKEN.finditer(s):
        tok = m.group(0)
        if tok.startswith('"'):
            color, kind = (KEY, 'mono') if m.group(1) else (STR, 'mono')
        elif tok[0].isdigit() or tok in ('true', 'false', 'null'):
            color, kind = LIT, 'mono'
        else:
            color, kind = PUNCT, 'mono'
        c.text(x, y, tok, kind, size, color)
        x += c.tw(tok, kind, size)


def slide2():
    c = Canvas(1600, 900)
    frame(c, "Claude as a process: one conversation over a pipe",
          "ai-reviewer starts one claude per document and keeps its stdin open. A comment goes in as one JSON line; events stream back until result.")

    # --- browser -------------------------------------------------------------
    c.rrect(60, 205, 160, 110, r=12, fill=SLATE_BG, stroke=SLATE, sw=2)
    c.text(140, 245, "browser", 'b', 19, INK, anchor='mm')
    c.text(140, 272, "the Gren app", 'r', 14, SUB, anchor='mm')
    c.arrow([(220, 260), (300, 260)], SLATE, 2.4, head=10, both=True)
    c.text(260, 240, "WebSocket", 'm', 12, SLATE, anchor='mm')

    # --- ai-reviewer ---------------------------------------------------------
    AX, AY, AW, AH = 300, 145, 440, 265
    c.rrect(AX, AY, AW, AH, r=14, fill=PROG_BG, stroke=PROG, sw=2.2)
    c.text(AX + 22, AY + 30, "ai-reviewer", 'b', 21, INK, anchor='lm')
    c.text(AX + 22 + c.tw("ai-reviewer", 'b', 21) + 12, AY + 31, "the Go daemon", 'r', 15, SUB, anchor='lm')

    def row(y, title, sub):
        c.rrect(AX + 20, y, AW - 40, 58, r=9, fill=WHITE, stroke=PROG_SOFT, sw=1.8)
        c.text(AX + 36, y + 20, title, 'm', 15, INK, anchor='lm')
        c.text(AX + 36, y + 41, sub, 'r', 13, SUB, anchor='lm')
    row(AY + 58,  "writes one user frame per comment", "the selected passage, quoted, plus the comment")
    row(AY + 124, "one reader on stdout, for the life of the process", "a streaming JSON decoder, not a line scanner")
    row(AY + 190, "watches the tool calls", "Edit / Write on a file: git add it; result: commit")

    # --- claude ----------------------------------------------------------------
    CX, CY, CW, CH = 1030, 153, 494, 257
    for k in (2, 1):                    # one per document under review
        c.rrect(CX + 8 * k, CY - 8 * k, CW, CH, r=14, fill=WHITE, stroke=CLAUDE_SOFT, sw=1.6)
    c.rrect(CX, CY, CW, CH, r=14, fill=CLAUDE_BG, stroke=CLAUDE, sw=2.2)
    c.text(CX + 22, CY + 30, "claude -p", 'monob', 21, INK, anchor='lm')
    w = c.tw("one per document", 'm', 13) + 22
    c.chip(CX + CW - 22 - w, CY + 18, "one per document", 'm', 13, WHITE, CLAUDE, pad=11, h=24, r=12)
    args = [
        ("--input-format stream-json",          "read messages from stdin, forever"),
        ("--output-format stream-json",         "emit events as JSON lines"),
        ("--verbose",                           "required with stream-json"),
        ("--permission-mode acceptEdits",       "never wait on a prompt"),
        ("--tools Read,Edit,Write,Grep,Glob",   "no Bash"),
        ("--session-id <uuid>",                 "first launch; --resume after"),
    ]
    for i, (a, why) in enumerate(args):
        y = CY + 68 + i * 31
        c.text(CX + 26, y, a, 'mono', 15, INK, anchor='lm')
        c.text(CX + CW - 22, y, why, 'r', 13, SUB, anchor='rm')

    # --- the pipes -------------------------------------------------------------
    L, R = AX + AW, CX
    mid = (L + R) / 2
    c.arrow([(L, 200), (R, 200)], PROG, 3.2, head=12)
    c.text(mid, 184, "stdin", 'b', 14, PROG, anchor='mm')
    c.text(mid, 218, "a user message, one line", 'r', 13, SUB, anchor='mm')
    c.arrow([(R, 270), (L, 270)], CLAUDE, 3.2, head=12)
    c.text(mid, 254, "stdout", 'b', 14, CLAUDE, anchor='mm')
    c.text(mid, 288, "events, one line each", 'r', 13, SUB, anchor='mm')
    c.arrow([(R, 345), (L, 345)], SUB, 2, head=10, dash=(7, 5))
    c.text(mid, 329, "stderr", 'm', 13, SUB, anchor='mm')
    c.text(mid, 363, "last 8 KB kept, for errors", 'r', 13, SUB, anchor='mm')

    # --- the request -----------------------------------------------------------
    PY = 455
    QX, QW = 60, 560
    c.rrect(QX, PY, QW, 395, r=12, fill=WASH, stroke=LINE, sw=1.5)
    c.circle(QX + 30, PY + 30, 13, fill=PROG)
    c.arrow([(QX + 23, PY + 30), (QX + 39, PY + 30)], WHITE, 2.2, head=8)
    c.text(QX + 54, PY + 30, "stdin: one comment", 'b', 18, INK, anchor='lm')
    c.text(QX + 24, PY + 60, "One line on the wire, laid out here. The Messages API user shape.", 'r', 13.5, SUB, anchor='lm')
    req = [
        '{"type": "user",',
        ' "message": {',
        '   "role": "user",',
        '   "content": [{',
        '     "type": "text",',
        '     "text": "File: docs/spec.md\\n\\n',
        '              The reviewer selected this passage:\\n\\n',
        '              > it retries forever\\n\\n',
        '              Their comment:\\n\\n',
        '              > say it gives up after 60 s"',
        '   }]',
        ' }}',
    ]
    for i, ln in enumerate(req):
        json_line(c, QX + 24, PY + 84 + i * 21, ln)
    c.rrect(QX + 16, PY + 342, QW - 32, 34, r=8, fill=PROG_BG)
    c.text(QX + 28, PY + 359, "stdin stays open: the next comment goes to the same process", 'm', 13.5, PROG, anchor='lm')

    # --- the response ----------------------------------------------------------
    RX, RW = 650, 890
    c.rrect(RX, PY, RW, 395, r=12, fill=WASH, stroke=LINE, sw=1.5)
    c.circle(RX + 30, PY + 30, 13, fill=CLAUDE)
    c.arrow([(RX + 37, PY + 30), (RX + 21, PY + 30)], WHITE, 2.2, head=8)
    c.text(RX + 54, PY + 30, "stdout: what came of it", 'b', 18, INK, anchor='lm')
    c.text(RX + 24, PY + 60, "One event per line, abridged. Ignore any type you do not know; new ones appear.", 'r', 13.5, SUB, anchor='lm')
    frames = [
        ("system",    ['{"type":"system", "subtype":"init", "session_id":"7f3c…", "model":"claude-opus-5"}'], None),
        ("assistant", ['{"type":"assistant", "message":{"content":[{"type":"tool_use", "name":"Edit",',
                       ' "input":{"file_path":"…/docs/spec.md", "old_string":"it retries forever",',
                       '          "new_string":"it gives up after 60 s"}}]}}'],
                      "an Edit: the daemon notes the file for git add"),
        ("user",      ['{"type":"user", "message":{"content":[{"type":"tool_result",',
                       ' "content":"The file …/docs/spec.md has been updated successfully."}]}}'], None),
        ("assistant", ['{"type":"assistant", "message":{"content":[{"type":"text",',
                       ' "text":"Said it gives up after 60 s, as the code does."}]}}'], None),
        ("result",    ['{"type":"result", "subtype":"success", "is_error":false,',
                       ' "result":"Said it gives up after 60 s…", "total_cost_usd":0.031}'],
                      "the only end of a turn: commit, show the reply"),
    ]
    y = PY + 86
    for tag, lines, note in frames:
        tone = CLAUDE if tag in ("assistant", "result") else SLATE
        c.chip(RX + 22, y - 3, tag, 'm', 12.5, tone, WHITE, pad=8, h=22, r=11, stroke=LINE)
        for j, ln in enumerate(lines):
            json_line(c, RX + 112, y + j * 19, ln, 13)
        y += len(lines) * 19
        if note:
            c.arrow([(RX + 116, y + 3), (RX + 116, y + 10), (RX + 130, y + 10)], PROG, 1.6, head=6)
            c.text(RX + 136, y + 1, note, 'mi', 13, PROG)
            y += 19
        y += 9

    c.save(os.path.join(OUT, "slides-2-pipes.png"))


# --- slide 3 -------------------------------------------------------------------

def json_panel(c, x, y, w, h, n, title, lines, size=12.5, lh=17.5):
    c.rrect(x, y, w, h, r=12, fill=WASH, stroke=LINE, sw=1.5)
    c.badge(x + 28, y + 28, n, PROG if n != 2 else REMOTE, r=12)
    c.text(x + 50, y + 28, title, 'b', 16, INK, anchor='lm')
    yy = y + 56
    for ln in lines:
        if isinstance(ln, tuple):       # (kind, text): a caption, not JSON
            kind, text = ln
            if kind == 'http':
                c.text(x + 22, yy, text, 'monob', size, SLATE)
            else:
                c.arrow([(x + 26, yy + 2), (x + 26, yy + 9), (x + 40, yy + 9)], PROG, 1.6, head=6)
                c.text(x + 46, yy, text, 'mi', 13, PROG)
            yy += lh + 3
        else:
            json_line(c, x + 22, yy, ln, size)
            yy += lh


def slide3():
    c = Canvas(1600, 900)
    frame(c, "Underneath: a chat API over the network",
          "An LLM API takes a list of messages, each tagged with a role, and answers with the next one. Here is slide 2's comment sent to OpenAI.",
          "Claude Code drives Anthropic's version of this API, where a message is a list of blocks: `text`, `tool_use`, `tool_result`.",
          "stream-json shows you those messages as claude runs the loop. That is why a tool result comes back as a \"user\" frame.")

    # --- program, internet, model ----------------------------------------------
    TOP, BOT = 145, 340
    c.rrect(60, TOP + 30, 220, 130, r=14, fill=PROG_BG, stroke=PROG, sw=2.2)
    c.text(170, TOP + 68, "your program", 'b', 20, INK, anchor='mm')
    c.text(170, TOP + 96, "keeps the history", 'r', 14, SUB, anchor='mm')
    c.text(170, TOP + 116, "runs the tools", 'r', 14, SUB, anchor='mm')

    c.rrect(330, TOP, 300, BOT - TOP, r=40, fill=None, stroke=LINE, sw=1.6)
    c.text(480, TOP + 22, "the internet", 'm', 14, SUB, anchor='mm')
    c.arrow([(280, TOP + 72), (680, TOP + 72)], PROG, 2.8, head=12)
    c.text(480, TOP + 60, "HTTPS  POST /v1/chat/completions", 'monob', 13, PROG, anchor='mm')
    c.arrow([(680, TOP + 122), (280, TOP + 122)], REMOTE, 2.8, head=12)
    c.text(480, TOP + 140, "JSON: the next message", 'm', 13, REMOTE, anchor='mm')
    c.text(480, BOT - 28, "one request, one response", 'r', 13, SUB, anchor='mm')

    c.rrect(680, TOP + 30, 180, 130, r=14, fill=REMOTE_BG, stroke=REMOTE, sw=2.2)
    c.text(770, TOP + 68, "the model", 'b', 20, INK, anchor='mm')
    c.text(770, TOP + 96, "api.openai.com", 'mono', 14, SUB, anchor='mm')
    c.text(770, TOP + 116, "no files, no memory", 'r', 14, SUB, anchor='mm')

    # --- how slide 2 maps onto it ------------------------------------------------
    MX, MW = 900, 640
    side_panel(c, MX, TOP, MW, BOT - TOP, "Slide 2 is this loop, run for you by claude")
    c.text(MX + 24, TOP + 58, "this API", 'm', 13, SUB)
    c.text(MX + 300, TOP + 58, "claude -p, slide 2", 'm', 13, SUB)
    rows = [
        ("a user message",               "the user frame you write"),
        ("assistant tool_calls",         "an assistant frame with tool_use"),
        ("a tool message: you ran it",   "a user frame with tool_result: claude ran it"),
        ("finish_reason: \"stop\"",      "the result frame"),
        ("you resend the history",       "the process keeps it"),
    ]
    for i, (l, r) in enumerate(rows):
        y = TOP + 84 + i * 22
        c.text(MX + 24, y, l, 'r', 14, INK)
        c.arrow([(MX + 262, y + 9), (MX + 288, y + 9)], SUB, 1.6, head=7)
        c.text(MX + 300, y, r, 'r', 14, INK)

    # --- the exchange ------------------------------------------------------------
    PY, PH, GAP = 368, 402, 22
    PW = (1480 - 2 * GAP) / 3
    xs = [60 + i * (PW + GAP) for i in range(3)]

    json_panel(c, xs[0], PY, PW, PH, 1, "the request", [
        ('http', 'POST /v1/chat/completions'),
        '{"model": "gpt-4o",',
        ' "messages": [',
        '   {"role": "system",',
        '    "content": "You are editing a document…"},',
        '   {"role": "user",',
        '    "content": "File: docs/spec.md\\n\\n',
        '      The reviewer selected this passage:\\n\\n',
        '      > it retries forever\\n\\n',
        '      Their comment:\\n\\n',
        '      > say it gives up after 60 s"}',
        ' ],',
        ' "tools": [{"type": "function",',
        '   "function": {"name": "edit",',
        '     "parameters": {"type": "object",',
        '       "properties": {',
        '         "old_string": {"type": "string"},',
        '         "new_string": {"type": "string"}}}}}]}',
        ('note', "the tools are only described; you run them"),
    ])

    json_panel(c, xs[1], PY, PW, PH, 2, "the response: a tool call", [
        ('http', '200 OK'),
        '{"choices": [{',
        '   "message": {',
        '     "role": "assistant",',
        '     "content": null,',
        '     "tool_calls": [{',
        '       "id": "call_7Qx…",',
        '       "type": "function",',
        '       "function": {',
        '         "name": "edit",',
        '         "arguments": "{\\"old_string\\": \\"it retries forever\\",',
        '            \\"new_string\\": \\"it gives up after 60 s\\"}"',
        '       }}]},',
        '   "finish_reason": "tool_calls"}],',
        ' "usage": {"prompt_tokens": 412,',
        '           "completion_tokens": 38}}',
        ('note', "the model cannot edit a file; it asks you to"),
    ])

    json_panel(c, xs[2], PY, PW, PH, 3, "run edit, then ask again", [
        ('http', 'POST /v1/chat/completions'),
        '{"model": "gpt-4o",',
        ' "messages": [',
        '   {"role": "system", "content": "You are…"},',
        '   {"role": "user", "content": "File: docs/…"},',
        '   {"role": "assistant", "tool_calls": […]},',
        '   {"role": "tool", "tool_call_id": "call_7Qx…",',
        '    "content": "Edited docs/spec.md"}],',
        ' "tools": […]}',
        ('note', "the whole conversation, every time"),
        ('http', '200 OK'),
        '{"choices": [{',
        '   "message": {"role": "assistant",',
        '     "content": "Said it gives up after 60 s."},',
        '   "finish_reason": "stop"}]}',
        ('note', "stop: the turn is over"),
    ])

    # the order of play, in the gutters
    for i in range(2):
        gx = xs[i] + PW + GAP / 2
        c.circle(gx, PY + 28, 10, fill=WHITE, stroke=LINE, sw=1.4)
        c.arrow([(gx - 5, PY + 28), (gx + 6, PY + 28)], SUB, 1.8, head=6)

    c.save(os.path.join(OUT, "slides-3-chat-api.png"))


if __name__ == "__main__":
    slide1()
    slide2()
    slide3()
