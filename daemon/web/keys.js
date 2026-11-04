// keys.js: keys the browser terminal sends differently from xterm.js, so the
// web lens types like the Mac app's terminal (SwiftTerm).
//
// Shift+Enter: xterm.js sends a plain Enter (CR) for it, so Claude Code
// submits the prompt instead of starting a new line. SwiftTerm speaks the
// kitty keyboard protocol and sends Shift+Enter as its own key; xterm.js does
// not. Ctrl+J (LF) is what Claude Code takes as a new line in every terminal
// (code.claude.com/docs/en/terminal-config), so Shift+Enter sends that. A
// plain shell reads LF as Enter, as before.
//
// Cmd+Left / Cmd+Right: xterm.js sends nothing for them. SwiftTerm sends the
// Emacs word moves, ESC b and ESC f (MacTerminalView moveToLeftEndOfLine), so
// in the Mac app they jump a word back and forward. Same here.
(function (root) {
  "use strict";

  // Which modifiers a mapped key needs, exactly: any other held makes it a
  // different key, left to xterm.js.
  const KEYS = [
    { key: "Enter", mods: "shift", send: "\n" },
    { key: "ArrowLeft", mods: "meta", send: "\x1bb" },
    { key: "ArrowRight", mods: "meta", send: "\x1bf" },
  ];

  const modsOf = (e) => ["ctrl", "alt", "meta", "shift"].filter((m) => e[m + "Key"]).join("+");

  // inputFor is what a key event sends instead of xterm.js's own bytes, or
  // null to leave the key to xterm.js. It answers for keydown, keypress and
  // keyup alike, so the caller can swallow all three and send on keydown only.
  function inputFor(e) {
    if (!e || e.isComposing) return null;
    const hit = KEYS.find((k) => k.key === e.key && k.mods === modsOf(e));
    return hit ? hit.send : null;
  }

  // attach hooks inputFor into an xterm.js terminal; send takes the bytes.
  function attach(term, send) {
    term.attachCustomKeyEventHandler((e) => {
      const data = inputFor(e);
      if (data === null) return true;
      if (e.type === "keydown") {
        e.preventDefault(); // neither the hidden textarea nor the browser acts on it
        send(data);
      }
      return false;
    });
  }

  const api = { inputFor, attach };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  root.ccmuxKeys = api;
})(typeof window !== "undefined" ? window : this);
