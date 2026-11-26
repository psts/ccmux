// threadnotesctl.js: a repo's Notes on the thread page, from the notepad
// button in the header: a big pop-up over the dimmed page with one text
// box. They are the old lens's Scratchpad (scratchpad.js), the same text:
// one copy per workspace on the daemon, shared by everyone. What is saved
// when, the drafts and the status line are notes.js; this file is the
// pop-up and its requests.
//
// It opens scrolled to the end with the caret there, and closes on Esc,
// a click on the dim area or ×, saving first.
(() => {
  "use strict";
  const N = window.ccmuxNotes;
  const enc = encodeURIComponent;

  // create wires it to the page: d.call is its fetch (GET), d.fetch the
  // plain one (a save that must outlive the page asks for keepalive),
  // d.user() the name a save is signed with, d.repo() the repo on screen
  // ({ id, name } of its own workspace) or null, d.overlay() and d.sheet()
  // its elements, d.storage() this browser's localStorage or null,
  // d.later(fn, ms) and d.cancel(t) its timer, d.listen(type, fn) the
  // window's events.
  function create(d) {
    let box = null; // the sheet's parts, made at the first open
    const url = (id) => `/v1/workspaces/${enc(id)}/scratchpad?user=${enc(d.user())}`;
    const isOpen = () => !d.overlay().classList.contains("hidden");

    async function put(id, text, unloading) {
      const body = JSON.stringify({ content: text });
      const r = await d.fetch(url(id), { method: "PUT", keepalive: N.keepalive(unloading, body), headers: { "Content-Type": "application/json" }, body });
      const data = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(data.error || `HTTP ${r.status}`);
      return data;
    }

    function status(text, bad) {
      parts().status.textContent = text;
      parts().status.classList.toggle("bad", !!bad);
    }

    const notes = N.create({
      area: () => parts().area, status, put, get: (id) => d.call("GET", url(id)),
      storage: d.storage, later: d.later, cancel: d.cancel, open: isOpen,
      focused: () => document.activeElement === parts().area, when: (ms) => N.shortWhen(ms),
    });

    const el = (tag, cls, text) => {
      const n = document.createElement(tag);
      if (cls) n.className = cls;
      if (text) n.textContent = text;
      return n;
    };

    // parts makes the sheet once: its head, the box, the status line.
    function parts() {
      if (box) return box;
      const sheet = d.sheet();
      const head = el("div", "sheet-head notes-head");
      const title = el("h2", "sheet-title", "Notes");
      const sub = el("span", "notes-sub");
      const x = el("button", "sheet-close", "✕");
      Object.assign(x, { type: "button", title: "Close", onclick: close });
      x.setAttribute("aria-label", "Close");
      head.append(title, sub, x);
      const area = el("textarea", "notes-text");
      Object.assign(area, { spellcheck: false, placeholder: "Notes for this repo, shared with everyone…" });
      area.setAttribute("aria-label", "Notes");
      area.addEventListener("input", () => notes.typed());
      area.addEventListener("blur", () => notes.flush());
      const line = el("div", "notes-status");
      line.setAttribute("role", "status");
      sheet.textContent = "";
      sheet.append(head, area, line);
      box = { sub, area, status: line };
      return box;
    }

    // open shows the notes of the repo on screen. Unsaved typing on the
    // same repo stays; anything else is read fresh.
    async function open() {
      const repo = d.repo();
      if (!repo) return;
      const p = parts();
      p.sub.textContent = `${repo.name} · shared with everyone`;
      d.overlay().classList.remove("hidden");
      if (!notes.holds(repo.id)) await notes.load(repo.id, () => isOpen() && current(repo.id));
      if (isOpen() && current(repo.id)) toEnd(p.area);
    }
    // current is whether the repo on screen is still id: a load that lands
    // after the page moved on is not shown.
    const current = (id) => { const r = d.repo(); return !!r && r.id === id; };

    // toEnd puts the caret after the last line and scrolls there: notes
    // grow at the bottom. Scrolled last, once the text and focus are in.
    function toEnd(area) {
      area.focus();
      const n = area.value.length;
      area.setSelectionRange(n, n);
      area.scrollTop = area.scrollHeight;
    }

    function close() {
      if (!isOpen()) return;
      notes.flush();
      d.overlay().classList.add("hidden");
    }
    const escape = () => { if (!isOpen()) return false; close(); return true; };

    // A click on the dim area closes; a drag that started in the box and
    // let go outside it (selecting text) does not.
    let downOnDim = false;
    d.overlay().onpointerdown = (e) => { downOnDim = e.target === d.overlay(); };
    d.overlay().onclick = (e) => { if (e.target === d.overlay() && downOnDim) close(); downOnDim = false; };
    d.listen("pagehide", () => { if (notes.dirty()) notes.save(true); });

    return { open, close, escape, isOpen, remote: (id) => notes.remote(id) };
  }

  window.ccmuxThreadNotesCtl = { create };
})();
