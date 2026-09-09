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
      // The daemon restarts often during development, and the session cookie
      // outlives the socket, so reconnecting is nearly always the right move.
      window.setTimeout(connect, backoff);
      backoff = Math.min(backoff * 2, MAX_BACKOFF);
    };

    socket.onerror = function () {
      if (socket) socket.close();
    };
  }

  app.ports.socketOut.subscribe(function (frame) {
    if (socket && socket.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify(frame));
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

  function captureSelection() {
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

    var text = block.textContent;
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

  document.addEventListener("keyup", function (event) {
    if (event.shiftKey || event.key === "Escape") scheduleCapture();
  });

  app.ports.clearSelection.subscribe(function () {
    var sel = window.getSelection();
    if (sel) sel.removeAllRanges();
  });
})();
