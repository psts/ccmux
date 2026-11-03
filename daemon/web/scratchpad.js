// scratchpad.js: a workspace's shared notes, behind the 📝 tab after the pane
// tabs, shown in place of the terminal. One copy per workspace, kept on the
// daemon this page talks to (/v1/workspaces/{id}/scratchpad), the same for
// everyone. The last save wins, as it did in the Mac app; a save made
// elsewhere reaches an open page through the firehose and replaces the text
// unless this page has unsaved typing of its own.
//
// Showing it is a class on #app (style.css hides the terminal and the views
// that sit over it), so the attach that a reconnect or a hello re-runs can
// never put the terminal back on top of it.
// Runs after app.js and uses its globals ($, state, getUser, scheduleFit,
// renderTabs) at call time.
(() => {
  "use strict";
  const SAVE_MS = 700;
  let showing = false;
  let wsId = null; // whose notes the text box holds; null until a load lands
  let saved = ""; // the text the daemon last confirmed for wsId
  let timer = null;

  const area = () => $("scratchpad").querySelector("textarea");
  const dirty = () => wsId !== null && area().value !== saved;
  const url = (id) => `/v1/workspaces/${id}/scratchpad?user=${encodeURIComponent(getUser())}`;

  // Drafts: what is typed is also kept in this browser until the daemon
  // confirms it, so text a failed save could not deliver survives opening
  // another session's notes, a reload or a closed tab. A draft remembers the
  // notes it was typed on top of (base) and when. Opening those notes again
  // puts it back and saves it; if someone has saved since, their notes stay
  // and the draft is added below them, so neither is lost. Storage that
  // refuses (a private window) only loses that safety net, so it is logged.
  const draftKey = (id) => "ccmux-scratch-draft:" + id;
  function readDraft(id) {
    try {
      const raw = localStorage.getItem(draftKey(id));
      return raw === null ? null : JSON.parse(raw);
    } catch (e) {
      console.warn("scratchpad: draft unreadable:", e);
      return null;
    }
  }
  function keepDraft(id, text, base) {
    try {
      localStorage.setItem(draftKey(id), JSON.stringify({ text, base, at: Date.now() }));
    } catch (e) { console.warn("scratchpad: draft not kept:", e); }
  }
  // dropDraft forgets the draft once the daemon holds its exact text; a
  // draft typed on since then stays.
  function dropDraft(id, text) {
    const d = readDraft(id);
    if (!d || d.text !== text) return;
    try { localStorage.removeItem(draftKey(id)); } catch (e) { console.warn("scratchpad: draft not cleared:", e); }
  }

  function status(text, bad) {
    const s = $("scratchpad").querySelector(".sp-status");
    s.textContent = text;
    s.classList.toggle("bad", !!bad);
  }

  function wire() {
    const box = $("scratchpad");
    if (box.dataset.built) return;
    box.dataset.built = "1";
    box.innerHTML = `<div class="sp-head"><span class="sp-title">📝 Scratchpad</span>` +
      `<span class="sp-note">shared with everyone in this workspace</span><span class="sp-status"></span></div>` +
      `<textarea spellcheck="false" placeholder="Notes for this workspace…"></textarea>`;
    const ta = area();
    ta.addEventListener("input", () => {
      if (wsId !== null) keepDraft(wsId, ta.value, saved);
      status("Not saved yet");
      clearTimeout(timer);
      timer = setTimeout(save, SAVE_MS);
    });
    ta.addEventListener("blur", flush);
    window.addEventListener("pagehide", () => { if (dirty()) save(true); });
  }

  async function fetchPad(id) {
    const r = await fetch(url(id), { cache: "no-store" });
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  }

  // load reads the notes of workspace id into the box. A failed read leaves
  // the box read-only: typing into a blank that then saved would wipe them.
  async function load(id) {
    const ta = area();
    ta.readOnly = true;
    try {
      const pad = await fetchPad(id);
      if (state.wsId !== id) return; // moved on while it loaded
      wsId = id;
      saved = pad.content || "";
      ta.readOnly = false;
      showLoaded(id, pad);
    } catch (e) {
      wsId = null;
      ta.value = "";
      status("Couldn't load the notes: " + e.message, true);
    }
  }

  // showLoaded puts freshly read notes in the box, or this browser's draft
  // of them when it holds text the daemon never got, and saves that.
  function showLoaded(id, pad) {
    const ta = area(), d = readDraft(id);
    if (!d || d.text === saved) {
      if (d) dropDraft(id, d.text);
      if (ta.value !== saved) ta.value = saved;
      status(savedLine(pad));
      return;
    }
    ta.value = d.base === saved ? d.text : withDraft(saved, d);
    keepDraft(id, ta.value, saved); // what is saved next is this, not the old draft
    status(d.base === saved ? "Restored notes this browser had not saved yet"
      : "Newer notes were saved elsewhere; your unsaved text is added below them");
    clearTimeout(timer);
    timer = setTimeout(save, SAVE_MS);
  }

  // withDraft keeps both: the newer notes, then the draft under a line that
  // says what it is.
  function withDraft(newer, d) {
    const when = d.at ? new Date(d.at).toLocaleString() : "earlier";
    return `${newer}\n\n---- not saved from this browser, ${when} ----\n${d.text}`;
  }

  function savedLine(pad) {
    if (!pad.updatedAt) return "";
    const when = new Date(pad.updatedAt).toLocaleString();
    return `Saved ${when}` + (pad.updatedBy ? ` by ${pad.updatedBy}` : "");
  }

  // save sends the box when it differs from what the daemon holds. A failure
  // keeps the text and says so; the next keystroke or blur tries again.
  // unloading is the page going away: only then is the request asked to
  // outlive it, and only when small enough, since a browser refuses a
  // keepalive body over 64 KB outright.
  async function save(unloading) {
    clearTimeout(timer);
    if (!dirty()) return;
    const id = wsId, text = area().value;
    const body = JSON.stringify({ content: text });
    status("Saving…");
    try {
      const r = await fetch(url(id), {
        method: "PUT", keepalive: !!unloading && body.length < 60000,
        headers: { "Content-Type": "application/json" }, body,
      });
      if (!r.ok) throw new Error(await r.text());
      const pad = await r.json();
      dropDraft(id, text);
      if (wsId === id) { saved = text; status(dirty() ? "Not saved yet" : savedLine(pad)); }
    } catch (e) {
      if (wsId === id) status("Couldn't save: " + e.message, true);
    }
  }

  function flush() { if (dirty()) save(false); }

  // show puts the notes of the session on screen in place of its terminal.
  // Unsaved text for the same session stays; anything else is read fresh.
  function show() {
    if (!state.wsId) return;
    wire();
    showing = true;
    $("app").classList.add("scratch-open");
    renderTabs();
    if (!(wsId === state.wsId && dirty())) load(state.wsId);
    area().focus();
  }

  // hide goes back to the terminal, saving first.
  function hide() {
    if (!showing) return;
    flush();
    showing = false;
    $("app").classList.remove("scratch-open");
    renderTabs(); // the 📝 tab lets go now, not when the re-attach answers
    scheduleFit(); // the terminal has its box back; nothing observes that for it
  }

  // remote is a firehose "workspace-scratchpad": someone (this page too)
  // saved workspace id's notes. Read them while the box stays writable, and
  // take them only if nothing was typed here meanwhile; the caret stays put.
  async function remote(id) {
    const current = () => showing && id === wsId && !dirty();
    if (!current()) return;
    let pad;
    try {
      pad = await fetchPad(id);
    } catch (e) {
      if (current()) status("Couldn't read a change made elsewhere: " + e.message, true);
      return;
    }
    if (!current()) return;
    const ta = area(), at = [ta.selectionStart, ta.selectionEnd];
    saved = pad.content || "";
    if (ta.value !== saved) ta.value = saved;
    if (document.activeElement === ta) ta.setSelectionRange(at[0], at[1]);
    status(savedLine(pad));
  }

  window.ccmuxScratch = { show, hide, remote, isOpen: () => showing };
})();
