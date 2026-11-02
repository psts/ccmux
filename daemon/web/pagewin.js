// pagewin.js: the ccmux window THIS browser window shows. The rules live in
// route.js; this file wires them to the page: the address bar, the sidebar's
// "current window", relaunch restore, and the map the service worker reads to
// send a notification tap to the browser window that owns the session.
//
// The Mac app's WorkspaceWindowController is the same idea: one window, one
// ccmux window, its sessions first in the sidebar, the last one shown again.
// Runs after app.js and uses its globals (state, openWorkspace, openWindow,
// openHereOf, openDrawer, renderList, detachIfCurrent, $) at call time.
(function () {
  "use strict";
  const R = window.ccmuxRoute;
  const store = window.localStorage;
  const WS_WINDOWS = "/__ccmux_ws_windows"; // ws id -> window id, for sw.js
  const ALIVE_WAIT_MS = 250;

  const uuid = () => (crypto.randomUUID && crypto.randomUUID()) || String(Date.now()) + Math.random().toString(36).slice(2);
  // Per browser window, kept in window.name: it survives a reload of this
  // window and is never handed to a window it opens. Not sessionStorage: a
  // window opened with window.open gets a COPY of it, so a ChartLabs window
  // opened from the Mixed one took Mixed's id, overwrote its entry, and a
  // relaunch brought back only one of the two (seen in a real browser).
  const PAGE_PREFIX = "ccmux-page:";
  const pageId = (() => {
    if (!String(window.name || "").startsWith(PAGE_PREFIX)) window.name = PAGE_PREFIX + uuid();
    return window.name.slice(PAGE_PREFIX.length);
  })();

  // The address as the page was opened, read before anything can rewrite it:
  // a notification that launches the app attaches from push.js before the
  // window list has arrived, and that attach must not cost the boot its target.
  const opened = R.parse(location.search);
  // A reload of this browser window, not a new one: its page id was already
  // recorded. Read now, before this load's own first beat records it again.
  const reloaded = R.known(store, pageId);
  let booted = false; // the address is ours to rewrite only once boot has used it
  let waiting = false; // boot asked before the window list could be read

  let current = "";   // window id shown here; "" is the picker
  // Back to the picker only once this window has been SEEN open here: a read
  // started before the open was recorded must not bounce the window just shown.
  let seenOpen = false;
  let mapWritten = "";
  let mapFailing = false; // logged on the way INTO failure only: this runs on every refresh

  // Other browser windows of this app answer a ping, so a launch can tell a
  // relaunch (nobody answers) from Cmd+N (somebody does).
  const chan = typeof BroadcastChannel === "function" ? new BroadcastChannel("ccmux-pages") : null;
  const pongs = new Set();
  if (chan) {
    chan.onmessage = (e) => {
      const m = e.data || {};
      if (m.type === "ping" && m.from !== pageId) chan.postMessage({ type: "pong", from: pageId, to: m.from });
      if (m.type === "pong" && m.to === pageId) pongs.add(m.from);
    };
  }
  function othersAlive() {
    if (!chan) return Promise.resolve(new Set());
    pongs.clear();
    chan.postMessage({ type: "ping", from: pageId });
    return new Promise((resolve) => setTimeout(() => resolve(new Set(pongs)), ALIVE_WAIT_MS));
  }

  const windowById = (id) => (state.windows || []).find((w) => w.id === id);
  const isMember = (w, ws) => !!(windowById(w) && (windowById(w).workspaceIds || []).includes(ws));
  // showable says whether session id can be on screen in window w (w may be ""
  // for the picker): one of w's own sessions, or a live session in no window
  // (AVAILABLE), which shows inside whatever window this browser window has,
  // as on the Mac. A session the daemon no longer lists is neither.
  const isLoose = (id) => (state.workspaces || []).some((x) => x.id === id) && !R.windowOf(state.windows, id);
  const showable = (w, id) => !!id && (isMember(w, id) || isLoose(id));

  // The session stays in the address whenever it can be shown here, so a
  // reload lands on it again.
  function syncAddress() {
    if (!booted) return;
    const ws = showable(current, state.wsId) ? state.wsId : "";
    try { history.replaceState(null, "", R.href(current, ws)); } catch (_) {}
  }

  function show(w) {
    current = w || "";
    seenOpen = false;
    const win = windowById(current);
    document.title = win ? `${win.name} · ccmux` : "ccmux";
    R.beat(store, pageId, current, Date.now());
    syncAddress();
    renderList();
  }

  // leaveBoard takes down an attention board that belongs to another window:
  // only openWorkspace closes it otherwise, and switching windows with no
  // session to show would leave the old window's board on screen.
  function leaveBoard(name) {
    const other = state.board && state.board.toLowerCase() !== String(name || "").toLowerCase();
    if (other && window.ccmuxBoard) window.ccmuxBoard.close();
  }

  function picker() {
    leaveBoard("");
    if (state.wsId) detachIfCurrent(state.wsId);
    show("");
    openDrawer();
  }

  // enter puts window w on screen here: marked open for this lens first when a
  // link or a restore names one it has closed, then its session.
  async function enter(w, ws) {
    const win = windowById(w);
    if (!win) return picker();
    if (!openHereOf(win) && !(await openWindow(win))) return picker();
    leaveBoard(win.name);
    show(w);
    showSession(w, ws);
  }

  // showSession picks window w's session: the one asked for, else the one last
  // shown in it, else none (the session list opens instead). A deleted one is
  // not showable, so it falls to the one last shown.
  function showSession(w, ws) {
    const pick = [ws, R.recall(store, w)].find((id) => showable(w, id));
    if (pick === state.wsId && pick) return onAttach(pick); // already on screen (opened while boot waited)
    if (pick) return openWorkspace(pick, null);
    if (state.wsId && !showable(w, state.wsId)) return detachIfCurrent(state.wsId);
    openDrawer();
  }

  // A launch with no window in the address: Cmd+N while others are open, or a
  // reload of the picker, shows the picker; a relaunch brings back what was
  // open. A browser may refuse to open windows nobody clicked for; those get a
  // button each.
  async function launch() {
    if (reloaded) return picker();
    const live = await othersAlive();
    if (live.size) return picker();
    const back = R.takeRestoreSet(store, pageId, live).filter(windowById);
    if (!back.length) {
      const open = (state.windows || []).filter(openHereOf);
      return open.length === 1 ? enter(open[0].id, "") : picker();
    }
    await enter(back[0], "");
    const refused = back.slice(1).filter((id) => !window.open(R.href(id, ""), "_blank"));
    if (refused.length) offerRestore(refused);
  }

  function offerRestore(ids) {
    const bar = document.createElement("div");
    bar.id = "restore-bar";
    bar.innerHTML = "<span>Reopen windows from last time:</span>";
    for (const id of ids) {
      const b = document.createElement("button");
      b.textContent = (windowById(id) || {}).name || id;
      b.onclick = () => {
        window.open(R.href(id, ""), "_blank");
        b.remove();
        if (!bar.querySelector("button:not(.restore-dismiss)")) bar.remove();
      };
      bar.appendChild(b);
    }
    const x = document.createElement("button");
    x.className = "restore-dismiss";
    x.textContent = "×";
    x.onclick = () => bar.remove();
    bar.appendChild(x);
    document.body.appendChild(bar);
  }

  // boot decides what this browser window shows, and only from a window list
  // the daemon actually answered: an empty list from a failed read would send
  // a tap's session to the picker and spend the relaunch list on nothing.
  // Until a good read lands (onRefresh, every few seconds) the tapped session
  // opens on its own, as old ?ws= links did, and the address stays untouched.
  async function boot() {
    if (!state.windowsKnown) {
      if (!waiting) opened.ws ? openWorkspace(opened.ws, null) : openDrawer();
      waiting = true;
      return;
    }
    waiting = false;
    booted = true;
    // A session a tap opened while the page was starting outranks the address:
    // the address is where the page was, the tap is where the user asked to go.
    const w = opened.w;
    const ws = state.wsId || opened.ws;
    const target = R.bootTarget(state.windows, w, ws);
    if (target) return enter(target, ws);
    if (!ws) return launch();
    // No window to show: the session alone (already on screen when a tap
    // opened it while the page was starting).
    if (ws !== state.wsId) openWorkspace(ws, null);
  }

  // onAttach follows a session into its window: a click on another window's
  // row, or a notification for it, moves this browser window there. A session
  // in no window stays in this one.
  function onAttach(wsId) {
    const w = R.windowOf(state.windows, wsId);
    if (w && w !== current) show(w);
    R.remember(store, current, wsId);
    syncAddress();
  }

  // onRefresh runs after every window-list read.
  function onRefresh() {
    if (waiting && state.windowsKnown) boot();
    R.beat(store, pageId, current, Date.now());
    writeWindowMap();
    if (!current) return;
    const win = windowById(current);
    if (win && openHereOf(win)) seenOpen = true;
    else if (!win || seenOpen) picker(); // deleted, or closed from another window of this app
  }

  function writeWindowMap() {
    const map = {};
    for (const w of state.windows || []) for (const id of w.workspaceIds || []) map[id] = w.id;
    const json = JSON.stringify(map);
    if (json === mapWritten || typeof caches === "undefined") return;
    mapWritten = json;
    caches.open("ccmux-nav").then((c) => c.put(WS_WINDOWS, new Response(json)))
      .then(() => { mapFailing = false; })
      .catch((e) => {
        if (!mapFailing) console.warn("window map write failed; a notification tap will go to the focused window:", e);
        mapFailing = true;
        mapWritten = "";
      });
  }

  // openClosed opens a closed window from the sidebar. On the picker it lands
  // here; a browser window already showing one gets a new browser window, as
  // the Mac app opens a new window for it, unless the browser refuses one.
  async function openClosed(win) {
    if (!(await openWindow(win))) return;
    if (current && window.open(R.href(win.id, ""), "_blank")) return;
    enter(win.id, "");
  }

  function onClosed(id) {
    if (id === current) picker();
  }

  window.addEventListener("pagehide", () => R.ended(store, pageId, Date.now()));
  window.addEventListener("pageshow", () => R.beat(store, pageId, current, Date.now()));

  window.ccmuxPage = { current: () => current, boot, enter, onAttach, onRefresh, onClosed, openClosed, syncAddress };
})();
