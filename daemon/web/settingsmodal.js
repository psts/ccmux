// settingsmodal.js: the Settings window, one piece of code for both pages
// (the lens and the thread page). This file is the frame only: the window,
// its tab bar, opening and closing, and the shells of the editors that open
// above it. It makes its own markup, so no page copies it. Each tab is its
// own file that calls addTab with its page's markup and then wires it; a
// page gets a tab by loading that file. The look is settings.css, every rule
// of which hangs off the ROOT class below. Loads after dom.js.
(() => {
  "use strict";
  const ROOT = "ccmux-settings";
  const tabs = []; // { page, btn, sec, opened, closed }, in the order they joined
  const editors = []; // element ids of the editor windows, in the order they joined

  const shell =
    `<div id="settings-modal" class="modal ${ROOT} hidden">` +
    `<div class="sheet settings-sheet">` +
    `<header><span>Settings</span><button id="settings-close" title="Close">&times;</button></header>` +
    `<nav id="settings-tabs" class="settings-tabs"></nav>` +
    `<div class="settings-body"></div>` +
    `</div></div>`;

  // The shell of a window that opens above Settings to edit one thing (an
  // account, an agent): a title, a body its owner fills, a state line, Save.
  const editorShell = (name, sheet, title) =>
    `<div id="${name}-modal" class="modal agent-modal ${ROOT} hidden">` +
    `<div class="sheet ${sheet}">` +
    `<header><span id="${name}-modal-title">${title}</span><button id="${name}-modal-close" title="Close">&times;</button></header>` +
    `<div class="${name}-modal-body"></div>` +
    `<div class="sheet-actions agent-modal-actions">` +
    `<span id="${name}-modal-state" class="hint"></span>` +
    `<button id="${name}-modal-save" class="primary" type="button">Save</button>` +
    `</div></div></div>`;

  // One page at a time instead of one long scroll.
  function show(page) {
    for (const t of tabs) {
      t.btn.classList.toggle("active", t.page === page);
      t.sec.classList.toggle("hidden", t.page !== page);
    }
  }

  // addTab gives Settings one more tab: its name on the bar and its page,
  // made from html, which the caller then wires by id. opened runs each time
  // the window opens and closed each time it closes. Tabs stand in the order
  // their files load; first puts a page's own tab ahead of the shared ones.
  // The tab at the front is the one on show until another is picked.
  function addTab({ page, label, html = "", opened, closed, first = false }) {
    const btn = document.createElement("button");
    btn.className = "settings-tab";
    btn.type = "button";
    btn.dataset.page = page;
    btn.textContent = label;
    btn.onclick = () => show(page);
    const sec = document.createElement("section");
    sec.className = "settings-page";
    sec.dataset.page = page;
    sec.innerHTML = html;
    const bar = $("settings-tabs"), body = $("settings-modal").querySelector(".settings-body");
    if (first) { bar.prepend(btn); body.prepend(sec); tabs.unshift({ page, btn, sec, opened, closed }); }
    else { bar.appendChild(btn); body.appendChild(sec); tabs.push({ page, btn, sec, opened, closed }); }
    show(tabs[0].page);
    return sec;
  }

  // addEditor makes the shell of an editor window named name: its ids are
  // <name>-modal, -modal-title, -modal-close, -modal-state and -modal-save,
  // and its body is the element with class <name>-modal-body.
  function addEditor(name, sheet, title) {
    document.body.insertAdjacentHTML("beforeend", editorShell(name, sheet, title));
    editors.push(name + "-modal");
    return $(name + "-modal");
  }

  // Every tab hears an open and a close, whichever is on show: each reads
  // what it shows afresh, as each did on the gear's click before. One tab
  // failing must not keep the others from loading.
  function tell(what) {
    for (const t of tabs) {
      if (!t[what]) continue;
      try { t[what](); } catch (e) { console.error(`settings: the ${t.page} tab failed on ${what}`, e); }
    }
  }

  // open shows the window, on page when one is named, else where it was left.
  function open(page) {
    if (page) show(page);
    $("settings-modal").classList.remove("hidden");
    tell("opened");
  }

  function close() {
    $("settings-modal").classList.add("hidden");
    tell("closed");
  }

  const isOpen = () => !$("settings-modal").classList.contains("hidden");

  // escape closes the window on top, the way its own close button does: an
  // editor if one is open, else Settings. false when nothing was open, so
  // the page's Escape key can go on to mean something else.
  function escape() {
    for (const id of editors.slice().reverse()) {
      if ($(id).classList.contains("hidden")) continue;
      $(id + "-close").click();
      return true;
    }
    if (!isOpen()) return false;
    close();
    return true;
  }

  // read is GET /v1/settings on the hub. An error answer is NOT empty
  // settings: a 503 carries a JSON {"error":…} body, so parsing it succeeds
  // and every list is simply absent. The daemon's own text travels with the
  // refusal when it sent one, the status code when it did not.
  async function read() {
    const r = await fetch("/v1/settings");
    if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || `HTTP ${r.status}`);
    return r.json();
  }

  // put sends one part of the settings and answers with all of them, as the
  // daemon now holds them. A refusal throws the daemon's words.
  async function put(body) {
    const r = await fetch("/v1/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  }

  document.body.insertAdjacentHTML("beforeend", shell);
  const modal = $("settings-modal");
  $("settings-close").onclick = close;
  modal.onclick = (e) => { if (e.target === modal) close(); };
  // The one opener. The page's own Settings button is the frame's: no other
  // script hangs a handler on it.
  const opener = $("open-settings");
  if (opener) opener.onclick = () => open();

  window.ccmuxSettings = { addTab, addEditor, open, close, isOpen, escape, read, put };
})();
