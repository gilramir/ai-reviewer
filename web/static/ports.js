// Glue for the three ports Main.gren declares. Everything here is DOM and
// socket plumbing that Gren cannot express; no application logic lives in this
// file, and it holds no state the Gren model does not already own.

(function () {
  "use strict";

  var app = Gren.Main.init({
    node: document.getElementById("root"),
    flags: {},
  });

  // ---------------------------------------------------------------- socket

  var socket = null;
  var backoff = 500;
  var MAX_BACKOFF = 15000;

  // Frames written while the socket is down. A click during a reconnect should
  // take effect when the socket returns, not vanish.
  var pending = [];
  var MAX_PENDING = 64;

  function socketURL() {
    var scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
    return scheme + "//" + window.location.host + "/ws";
  }

  function connect() {
    app.ports.socketState.send("connecting");
    socket = new WebSocket(socketURL());

    socket.onopen = function () {
      backoff = 500;
      app.ports.socketState.send("open");

      var queued = pending;
      pending = [];
      for (var i = 0; i < queued.length; i++) {
        socket.send(JSON.stringify(queued[i]));
      }
    };

    socket.onmessage = function (event) {
      var frame;
      try {
        frame = JSON.parse(event.data);
      } catch (err) {
        return;
      }
      app.ports.socketIn.send(frame);
    };

    socket.onclose = function () {
      socket = null;
      app.ports.socketState.send("closed");

      // Sessions live in the daemon's memory, so a restart invalidates every
      // cookie and the upgrade is refused with 401 from then on. Retrying that
      // forever leaves a page whose buttons quietly do nothing, so find out
      // which kind of failure this is before deciding.
      hasSession(function (alive) {
        if (!alive) {
          window.location.assign("/login");
          return;
        }
        window.setTimeout(connect, backoff);
        backoff = Math.min(backoff * 2, MAX_BACKOFF);
      });
    };

    socket.onerror = function () {
      if (socket) socket.close();
    };
  }

  // A rejected upgrade and an unreachable daemon are indistinguishable from
  // the WebSocket API, so ask over HTTP, where the status code survives.
  function hasSession(done) {
    window
      .fetch("/session", { credentials: "same-origin", cache: "no-store" })
      .then(function (response) {
        done(response.status !== 401);
      })
      .catch(function () {
        // The daemon is unreachable rather than refusing us; keep retrying.
        done(true);
      });
  }

  app.ports.socketOut.subscribe(function (frame) {
    if (socket && socket.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify(frame));
      return;
    }
    if (pending.length < MAX_PENDING) {
      pending.push(frame);
    }
  });

  connect();

  // ------------------------------------------------------------- selection

  var CONTEXT = 48; // characters of prefix/suffix kept for re-anchoring

  // Walk up to the element the server gave an id to. Inline nodes carry ids
  // too, but anchoring to the enclosing block gives the server a larger,
  // more distinctive haystack to search when the document changes.
  function enclosingBlock(node) {
    var el = node.nodeType === Node.TEXT_NODE ? node.parentElement : node;
    while (el && el !== document.body) {
      if (el.dataset && el.dataset.spanStart !== undefined) return el;
      el = el.parentElement;
    }
    return null;
  }

  function offsetWithin(block, container, offset) {
    var range = document.createRange();
    range.selectNodeContents(block);
    range.setEnd(container, offset);
    return range.toString().length;
  }

  // The block's text as a selection would read it. Not textContent: a soft wrap
  // is a <br>, which contributes a newline to any selection across it and
  // nothing at all to textContent. Measuring the offset one way and slicing the
  // other puts the prefix and suffix a character off per line break, which is
  // what the server scores repeated passages by.
  function blockText(block) {
    var range = document.createRange();
    range.selectNodeContents(block);
    return range.toString();
  }

  // A comment being written lives in a textarea outside the document pane, and
  // a keystroke there is not a change of selection. Deciding that here matters:
  // an empty anchor closes the composer, so mistaking a keystroke for a
  // selection throws away whatever the reviewer had typed.
  function isTyping(el) {
    if (!el) return false;
    var tag = el.tagName;
    return tag === "TEXTAREA" || tag === "INPUT" || el.isContentEditable;
  }

  function captureSelection() {
    if (isTyping(document.activeElement)) return;

    var sel = window.getSelection();
    if (!sel || sel.rangeCount === 0 || sel.isCollapsed) {
      app.ports.selectionIn.send({
        nodeId: "",
        quote: "",
        prefix: "",
        suffix: "",
      });
      return;
    }

    var range = sel.getRangeAt(0);
    var pane = document.getElementById("doc-pane");
    if (!pane || !pane.contains(range.commonAncestorContainer)) return;

    var block = enclosingBlock(range.commonAncestorContainer);
    if (!block) return;

    var quote = sel.toString();
    if (!quote.trim()) return;

    var text = blockText(block);
    var start = offsetWithin(block, range.startContainer, range.startOffset);
    var end = start + quote.length;

    app.ports.selectionIn.send({
      nodeId: block.dataset.nodeId || "",
      quote: quote,
      prefix: text.slice(Math.max(0, start - CONTEXT), start),
      suffix: text.slice(end, end + CONTEXT),
    });
  }

  // Fire once the selection has settled rather than on every selectionchange,
  // which would rebuild the composer on each mouse move during a drag.
  function scheduleCapture() {
    window.setTimeout(captureSelection, 0);
  }

  document.addEventListener("mouseup", function (event) {
    var pane = document.getElementById("doc-pane");
    if (pane && pane.contains(event.target)) scheduleCapture();
  });

  // Only the keys that can actually move or extend a selection. The old test
  // was `event.shiftKey`, which is true of every capital letter and of "?", so
  // typing punctuation into the comment box read as a collapsed selection and
  // discarded the draft. Escape is handled in Gren, on the composer itself.
  var SELECTION_KEYS = {
    ArrowLeft: true,
    ArrowRight: true,
    ArrowUp: true,
    ArrowDown: true,
    Home: true,
    End: true,
    PageUp: true,
    PageDown: true,
    a: true, // select-all, with a modifier
  };

  document.addEventListener("keyup", function (event) {
    if (isTyping(event.target)) return;
    if (!Object.prototype.hasOwnProperty.call(SELECTION_KEYS, event.key)) return;
    if (event.key === "a" && !(event.ctrlKey || event.metaKey)) return;
    scheduleCapture();
  });

  app.ports.clearSelection.subscribe(function () {
    var sel = window.getSelection();
    if (sel) sel.removeAllRanges();
  });
})();
