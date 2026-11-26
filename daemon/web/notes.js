// notes.js: a repo's shared notes, decided the same way on both pages that
// show them: the old lens's Scratchpad tab (scratchpad.js) and the thread
// page's Notes pop-up (threadnotesctl.js). One copy per workspace, kept on
// the daemon (/v1/workspaces/{id}/scratchpad), the same for everyone. The
// last save wins; a save made elsewhere replaces the text on an open page
// unless that page has unsaved typing of its own.
//
// This file holds the decisions: when to save, what a draft kept in this
// browser does when the notes are read again, what the status line says.
// Each page brings its own text box, status line and requests (create's io),
// so neither page's fetch or markup lives here.
(() => {
  "use strict";
  const SAVE_MS = 700;
  // A browser refuses a keepalive body over 64 KB outright, so a page going
  // away asks for one only below that.
  const KEEPALIVE_MAX = 60000;
  const keepalive = (unloading, body) => !!unloading && body.length < KEEPALIVE_MAX;

  // Drafts: what is typed is also kept in this browser until the daemon
  // confirms it, so text a failed save could not deliver survives opening
  // other notes, a reload or a closed tab. A draft remembers the notes it
  // was typed on top of (base) and when. Both pages use the same key, so a
  // draft from one is found by the other. Storage that refuses (a private
  // window) only loses that safety net, so it is logged.
  const draftKey = (id) => "ccmux-scratch-draft:" + id;
  function drafts(storage) {
    const box = () => storage() || { getItem: () => null, setItem() {}, removeItem() {} };
    function read(id) {
      try {
        const raw = box().getItem(draftKey(id));
        return raw === null || raw === undefined ? null : JSON.parse(raw);
      } catch (e) {
        console.warn("notes: draft unreadable:", e);
        return null;
      }
    }
    function keep(id, text, base) {
      try { box().setItem(draftKey(id), JSON.stringify({ text, base, at: Date.now() })); } catch (e) { console.warn("notes: draft not kept:", e); }
    }
    // drop forgets the draft once the daemon holds its exact text; a draft
    // typed on since then stays.
    function drop(id, text) {
      const d = read(id);
      if (!d || d.text !== text) return;
      try { box().removeItem(draftKey(id)); } catch (e) { console.warn("notes: draft not cleared:", e); }
    }
    return { read, keep, drop };
  }

  const longWhen = (ms) => new Date(ms).toLocaleString();
  // shortWhen is a time for the status line: the clock alone today, the
  // date too before that.
  function shortWhen(ms, now = Date.now()) {
    const d = new Date(ms), today = new Date(now);
    if (d.toDateString() === today.toDateString()) return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    return d.toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
  }

  function savedLine(pad, when = longWhen) {
    if (!pad.updatedAt) return "";
    return `Saved ${when(pad.updatedAt)}` + (pad.updatedBy ? ` by ${pad.updatedBy}` : "");
  }

  // withDraft keeps both: the newer notes, then the draft under a line that
  // says what it is.
  function withDraft(newer, d, when = longWhen) {
    return `${newer}\n\n---- not saved from this browser, ${d.at ? when(d.at) : "earlier"} ----\n${d.text}`;
  }

  // opened is what freshly read notes (saved) show, given this browser's
  // draft of them (d, or null): the notes alone, or the draft put back when
  // it was typed on these very notes, or the notes with the draft added
  // below when someone saved since. A draft shown is saved at once.
  function opened(saved, d, when = longWhen) {
    if (!d || d.text === saved) return { text: saved, line: null, drop: !!d, save: false };
    const same = d.base === saved;
    return {
      text: same ? d.text : withDraft(saved, d, when), drop: false, save: true,
      line: same ? "Restored notes this browser had not saved yet" : "Newer notes were saved elsewhere; your unsaved text is added below them",
    };
  }

  // padOf takes the daemon's answer only when it is notes: anything else
  // (a list, a page) must never reach the box, where a save would write it.
  function padOf(x) {
    if (!x || typeof x !== "object" || Array.isArray(x) || typeof x.content !== "string") throw new Error("the daemon's answer is not notes");
    return x;
  }

  // create runs one page's notes. io is the page: area() its text box,
  // status(text, bad) its status line, get(id) and put(id, text, unloading)
  // its requests (each a promise of the daemon's answer), storage() this
  // browser's localStorage or null, later(fn, ms) and cancel(handle) its
  // timer, open() whether the notes are on screen, focused() whether the
  // box has the caret, when(ms) how it writes a time.
  function create(io) {
    const st = { id: null, saved: "", timer: null, gen: 0 };
    const D = drafts(io.storage);
    const when = io.when || longWhen;
    const dirty = () => st.id !== null && io.area().value !== st.saved;
    const soon = () => { io.cancel(st.timer); st.timer = io.later(() => save(false), SAVE_MS); };

    // typed is an input in the box: keep the draft, save after a pause.
    function typed() {
      if (st.id !== null) D.keep(st.id, io.area().value, st.saved);
      io.status("Not saved yet");
      soon();
    }

    // load reads the notes of workspace id into the box, unless wanted()
    // says the page moved on while it read. A failed read leaves the box
    // read-only: typing into a blank that then saved would wipe them.
    // Notes of another workspace leave the box at once, so nothing typed
    // there meanwhile is taken for these.
    async function load(id, wanted = () => true) {
      const ta = io.area(), gen = ++st.gen;
      ta.readOnly = true;
      if (st.id !== id) { st.id = null; ta.value = ""; }
      try {
        const pad = padOf(await io.get(id));
        if (gen !== st.gen || !wanted()) return false;
        Object.assign(st, { id, saved: pad.content });
        ta.readOnly = false;
        show(id, pad);
        return true;
      } catch (e) {
        if (gen !== st.gen) return false;
        st.id = null;
        ta.value = "";
        io.status("Couldn't load the notes: " + e.message, true);
        return false;
      }
    }

    function show(id, pad) {
      const ta = io.area(), d = D.read(id), o = opened(st.saved, d, when);
      if (ta.value !== o.text) ta.value = o.text;
      if (o.drop) D.drop(id, d.text);
      if (o.save) D.keep(id, o.text, st.saved); // what is saved next is this, not the old draft
      io.status(o.line || savedLine(pad, when));
      if (o.save) soon();
    }

    // save sends the box when it differs from what the daemon holds. A
    // failure keeps the text and says so; the next keystroke or blur tries
    // again. unloading is the page going away.
    async function save(unloading) {
      io.cancel(st.timer);
      if (!dirty()) return;
      const id = st.id, text = io.area().value;
      io.status("Saving…");
      try {
        const pad = padOf(await io.put(id, text, !!unloading));
        D.drop(id, text);
        if (st.id === id) { st.saved = text; io.status(dirty() ? "Not saved yet" : savedLine(pad, when)); }
      } catch (e) {
        if (st.id === id) io.status("Couldn't save: " + e.message, true);
      }
    }

    const flush = () => { if (dirty()) save(false); };

    // remote is a firehose "workspace-scratchpad": someone (this page too)
    // saved workspace id's notes. Read them while the box stays writable,
    // and take them only if nothing was typed here meanwhile; the caret
    // stays put.
    async function remote(id) {
      const current = () => io.open() && id === st.id && !dirty();
      if (!current()) return;
      let pad;
      try {
        pad = padOf(await io.get(id));
      } catch (e) {
        if (current()) io.status("Couldn't read a change made elsewhere: " + e.message, true);
        return;
      }
      if (!current()) return;
      const ta = io.area(), at = [ta.selectionStart, ta.selectionEnd];
      st.saved = pad.content;
      if (ta.value !== st.saved) ta.value = st.saved;
      if (io.focused()) ta.setSelectionRange(at[0], at[1]);
      io.status(savedLine(pad, when));
    }

    // holds says the box has unsaved typing on workspace id's notes, which
    // showing them again must keep rather than read over.
    const holds = (id) => st.id === id && dirty();
    return { typed, load, save, flush, remote, dirty, holds, id: () => st.id };
  }

  const api = { create, opened, withDraft, savedLine, shortWhen, keepalive, padOf, drafts, SAVE_MS };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  if (typeof window !== "undefined") window.ccmuxNotes = api;
})();
