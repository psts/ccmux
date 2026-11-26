// scratchpad.js: a workspace's shared notes, behind the Scratchpad tab after the pane
// tabs, shown in place of the terminal. One copy per workspace, kept on the
// daemon this page talks to (/v1/workspaces/{id}/scratchpad), the same for
// everyone. What is saved when, the drafts this browser keeps and the
// status line are notes.js, which the thread page's Notes pop-up shares;
// this file is the tab's box and its requests.
//
// Showing it is a class on #app (style.css hides the terminal and the views
// that sit over it), so the attach that a reconnect or a hello re-runs can
// never put the terminal back on top of it.
// Runs after app.js and notes.js and uses their globals ($, state, getUser,
// scheduleFit, renderTabs, ccmuxNotes) at call time.
(() => {
  "use strict";
  let showing = false;
  let notes = null; // notes.js's state for this tab, made with its box

  const area = () => $("scratchpad").querySelector("textarea");
  const url = (id) => `/v1/workspaces/${id}/scratchpad?user=${encodeURIComponent(getUser())}`;

  function status(text, bad) {
    const s = $("scratchpad").querySelector(".sp-status");
    s.textContent = text;
    s.classList.toggle("bad", !!bad);
  }

  async function get(id) {
    const r = await fetch(url(id), { cache: "no-store" });
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  }

  async function put(id, text, unloading) {
    const body = JSON.stringify({ content: text });
    const r = await fetch(url(id), {
      method: "PUT", keepalive: window.ccmuxNotes.keepalive(unloading, body),
      headers: { "Content-Type": "application/json" }, body,
    });
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  }

  function storage() { try { return localStorage; } catch (_) { return null; } }

  function wire() {
    const box = $("scratchpad");
    if (box.dataset.built) return;
    box.dataset.built = "1";
    box.innerHTML = `<div class="sp-head"><span class="sp-title">Scratchpad</span>` +
      `<span class="sp-note">shared with everyone in this workspace</span><span class="sp-status"></span></div>` +
      `<textarea spellcheck="false" placeholder="Notes for this workspace…"></textarea>`;
    notes = window.ccmuxNotes.create({
      area, status, get, put, storage, later: (fn, ms) => setTimeout(fn, ms), cancel: (t) => clearTimeout(t),
      open: () => showing, focused: () => document.activeElement === area(),
    });
    const ta = area();
    ta.addEventListener("input", () => notes.typed());
    ta.addEventListener("blur", () => notes.flush());
    window.addEventListener("pagehide", () => { if (notes.dirty()) notes.save(true); });
  }

  // show puts the notes of the session on screen in place of its terminal.
  // Unsaved text for the same session stays; anything else is read fresh.
  function show() {
    if (!state.wsId) return;
    wire();
    showing = true;
    $("app").classList.add("scratch-open");
    renderTabs();
    const id = state.wsId;
    if (!notes.holds(id)) notes.load(id, () => state.wsId === id);
    area().focus();
  }

  // hide goes back to the terminal, saving first.
  function hide() {
    if (!showing) return;
    notes.flush();
    showing = false;
    $("app").classList.remove("scratch-open");
    renderTabs(); // the Scratchpad tab lets go now, not when the re-attach answers
    scheduleFit(); // the terminal has its box back; nothing observes that for it
  }

  // remote is a firehose "workspace-scratchpad" (notes.js remote).
  function remote(id) { return notes ? notes.remote(id) : Promise.resolve(); }

  window.ccmuxScratch = { show, hide, remote, isOpen: () => showing };
})();
