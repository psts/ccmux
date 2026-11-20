// threadmention.js: "@" file mentions in the thread page's box, as Claude
// Code's prompt box offers them. Typing "@" and part of a name lists the
// session's files that match (GET /v1/sessions/{id}/files?q=, the daemon's
// git list; the SDK keeps Claude Code's own autocomplete to the CLI). A
// pick puts "@path " in place of what was typed; Claude reads the file.
(() => {
  "use strict";

  const WAIT_MS = 120; // typing pauses this long before the list is asked for

  // mentionAt is the "@word" being typed where the cursor is: {start, query}
  // with start the index of the "@", or null. An "@" inside a word (an
  // email) is not a mention.
  function mentionAt(text, caret) {
    const before = String(text).slice(0, caret);
    const m = /(^|\s)@([^\s@]*)$/.exec(before);
    if (!m) return null;
    return { start: before.length - m[2].length - 1, query: m[2] };
  }

  // pick is the box's text with the mention replaced by the chosen path,
  // and where the cursor goes after it.
  function pick(text, caret, at, file) {
    const put = "@" + file + " ";
    return { text: text.slice(0, at.start) + put + text.slice(caret).replace(/^\s+/, ""), caret: at.start + put.length };
  }

  // create wires it to the box: d.input and d.box are the elements, d.files(q)
  // the matching paths (a promise), d.later(fn, ms) the page's timer.
  function create(d) {
    const st = { at: null, files: [], on: 0, gen: 0 };

    const open = () => st.files.length > 0 && !d.box.classList.contains("hidden");
    function close() { st.files = []; st.at = null; d.box.classList.add("hidden"); }

    // changed reads the box again after a keystroke.
    function changed() {
      const at = mentionAt(d.input.value, d.input.selectionStart ?? d.input.value.length);
      const gen = ++st.gen;
      if (!at) { close(); return; }
      st.at = at;
      d.later(async () => {
        if (gen !== st.gen) return;
        const files = await d.files(at.query).catch(() => []);
        if (gen !== st.gen) return;
        Object.assign(st, { files, on: 0 });
        draw();
      }, WAIT_MS);
    }

    function draw() {
      d.box.textContent = "";
      d.box.classList.toggle("hidden", !st.files.length);
      st.files.forEach((f, i) => {
        const row = document.createElement("button");
        row.type = "button";
        row.className = "cmd" + (i === st.on ? " on" : "");
        const name = document.createElement("span");
        name.className = "cmd-name";
        name.textContent = "@" + f;
        row.appendChild(name);
        row.onmousedown = (e) => e.preventDefault(); // the box keeps the focus
        row.onclick = () => choose(i);
        d.box.appendChild(row);
      });
    }

    function choose(i) {
      const f = st.files[i];
      if (!f || !st.at) return;
      const out = pick(d.input.value, d.input.selectionStart ?? d.input.value.length, st.at, f);
      d.input.value = out.text;
      d.input.setSelectionRange?.(out.caret, out.caret);
      d.input.focus();
      close();
    }

    // key takes the keys the open list answers; true when it took one.
    function key(e) {
      if (!open()) return false;
      const moves = { ArrowDown: 1, ArrowUp: -1 };
      if (e.key in moves) {
        st.on = (st.on + moves[e.key] + st.files.length) % st.files.length;
        draw();
      } else if (e.key === "Enter" || e.key === "Tab") choose(st.on);
      else if (e.key === "Escape") close();
      else return false;
      e.preventDefault();
      return true;
    }

    return { changed, key, close, isOpen: open };
  }

  const api = { create, mentionAt, pick };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  if (typeof window !== "undefined") window.ccmuxThreadMention = api;
})();
