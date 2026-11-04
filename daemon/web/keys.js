// keys.js: keys the browser terminal sends differently from xterm.js.
//
// Shift+Enter: xterm.js sends a plain Enter (CR) for it, so Claude Code
// submits the prompt instead of starting a new line. The Mac app's terminal
// (SwiftTerm) speaks the kitty keyboard protocol and sends Shift+Enter as its
// own key; xterm.js does not. Ctrl+J (LF) is what Claude Code takes as a new
// line in every terminal (code.claude.com/docs/en/terminal-config), so
// Shift+Enter sends that. A plain shell reads LF as Enter, as before.
(function (root) {
  "use strict";

  // inputFor is what a key event sends instead of xterm.js's own bytes, or
  // null to leave the key to xterm.js. It answers for keydown, keypress and
  // keyup alike, so the caller can swallow all three and send on keydown only.
  function inputFor(e) {
    if (!e || e.isComposing || e.key !== "Enter") return null;
    if (!e.shiftKey || e.ctrlKey || e.altKey || e.metaKey) return null;
    return "\n";
  }

  // attach hooks inputFor into an xterm.js terminal; send takes the bytes.
  function attach(term, send) {
    term.attachCustomKeyEventHandler((e) => {
      const data = inputFor(e);
      if (data === null) return true;
      if (e.type === "keydown") {
        e.preventDefault(); // the hidden textarea must not take the line break
        send(data);
      }
      return false;
    });
  }

  const api = { inputFor, attach };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  root.ccmuxKeys = api;
})(typeof window !== "undefined" ? window : this);
