// ccmux web lens — a thin xterm.js client over the daemon's REST + WS API.
// No build step: vanilla JS against the globals Terminal (xterm.js) and
// FitAddon (addon-fit), served embedded from ccmuxd.
"use strict";

const state = {
  workspaces: [],
  windows: [],       // shared windows from GET /v1/windows: {id, name, open, openBy, workspaceIds}
  hosts: {},         // federation: host label -> {id, addr, ...} from GET /v1/hosts
  createHost: "",    // host chosen in the New-workspace picker ("" = hub/self)
  projectPath: "",   // folder the New-workspace picker is browsing (rel to root)
  wsId: null,
  paneId: null,      // currently rendered pane
  wantPane: null,    // pane to select after the next attach (for tab switches)
  panes: [],
  conn: null,
  term: null,
  fit: null,
  firehose: null,    // global /v1/events WS (sidebar attention, all workspaces)
  attn: {},          // wsId -> { paneId -> attentionState } from the firehose
  claims: {},        // wsId -> { paneId -> {state, reason, since} }: the same, with why and since when (board.js)
  board: "",         // the window whose attention board is on show, "" when none (board.js)
  tabDrag: null,     // pane id of the tab being dragged along the strip, else null
  paneCols: 0,       // authoritative width of the current pane (from the daemon)
  gitOpen: {},       // wsId -> true when the row's changed-files list is expanded
  harnessIcons: {},  // harness name -> its icon from Settings, for the tab strip
  iconsFor: null,    // the session harnessIcons was last read for
};

// $, esc and copyToClipboard are dom.js's, and who this lens is (whoami,
// currentName, getUser, whoAmILine) is identity.js's: both load before this
// file, and the thread page loads them too.

// Device id, not a person: two lenses under ONE login each hold their own
// window open flags, so a browser closing a window cannot clear the Mac's. The
// label is only for a human reading a stale row and is never matched on.
//
// MODULE scope on purpose: several functions below use it. Declared inside
// getUser() it was a ReferenceError from each of them, swallowed by
// fetchWorkspaces's catch, which left the whole sidebar empty.
const deviceId = (() => {
  let dev = localStorage.getItem("ccmux-device");
  if (!dev) {
    dev = (crypto.randomUUID && crypto.randomUUID()) || String(Date.now()) + Math.random().toString(36).slice(2);
    localStorage.setItem("ccmux-device", dev);
  }
  return dev;
})();

// "Patric (web)" reads better than a uuid when you are looking at which lens is
// holding a window open. Recomputed rather than stored: the name can change.
const deviceLabel = () => currentName() + " (web)";

// A phone is a SUPPORT lens: it mirrors what its person has open on their
// computers rather than keeping windows of its own. It lists every window open
// for the person (`open`, not `openHere`), holds nothing open by looking, has
// no close, and starts in the session they last typed in (pagewin.js). A
// window it does open is opened for the person, under a "phone:" device the
// daemon clears whenever any of their computers closes that window
// (store.PhoneDevicePrefix). Decided once at load: a narrow desktop window is
// not a phone, and the device id must not change under a running page.
const phoneLens = !!(window.matchMedia && window.matchMedia("(max-width: 700px) and (pointer: coarse)").matches);
window.ccmuxPhone = phoneLens;
// The device a window open or close is recorded under. Exported for the
// smoke test, the only reader outside this file.
const windowDevice = phoneLens ? "phone:" + deviceId : deviceId;
window.ccmuxWindowDevice = windowDevice;

// Does THIS lens hold the window open? `openHere` is per-device; `open` is
// per-login and cannot answer it. The fallback is for a daemon older than the
// field: without it a newer lens reads every window as closed, renders live
// windows as closed rows, and clicking one opens a second window onto a shared
// one. Module scope so the smoke test can assert it directly.
function openHereOf(w) {
  if (phoneLens) return !!w.open; // a phone mirrors its person's computers
  return w.openHere ?? w.open;
}

// This lens does NOT declare an open-set, and that is deliberate.
//
// open-set is the repair path: a lens states the whole set it has open and the
// daemon drops that device's rows for anything omitted. It only repairs when
// the lens has evidence the daemon lacks. The Mac has that — its on-screen
// window controllers. A browser has none: what it renders as open IS the
// daemon's `openHere`, so declaring it back can omit nothing and repairs
// nothing. If you are about to add one, give the lens real evidence first.
//
// It DOES send a keep-alive, which is a different thing. Every flag expires
// after store.OpenFlagTTL, and the only writers here are the user clicking open
// or close — so a tab left open and in use for longer than that would age out
// of its own flags, drop out of openBy, render its own live windows as closed,
// and let another lens compute last=true and force-archive sessions that are on
// screen. The Mac avoids this with its 12-hour re-send; this is the same rule.
//
// It goes through the per-window open route, which is ADDITIVE. That matters:
// it can never retract a row, so unlike a declaration it is harmless if it
// races a read or runs on stale state. A stranded tab sends nothing either way,
// so the TTL still reaches it — that case is untouched.
let lastKeepAliveAt = 0;
async function keepOpenFlagsAlive() {
  if (phoneLens) return; // a phone holds nothing open by looking
  if (Date.now() - lastKeepAliveAt < 12 * 3600 * 1000) return;
  lastKeepAliveAt = Date.now();
  for (const w of (state.windows || []).filter(openHereOf)) {
    try {
      const r = await fetch(
        `/v1/windows/${w.id}/open?device=${encodeURIComponent(windowDevice)}` +
        `&deviceLabel=${encodeURIComponent(deviceLabel())}`, { method: "POST" });
      if (!r.ok) {
        lastKeepAliveAt = 0; // released, so the next read retries
        console.error("open-flag keep-alive failed:", w.id, r.status, await r.text());
      }
    } catch (e) {
      lastKeepAliveAt = 0;
      console.error("open-flag keep-alive failed:", w.id, e);
    }
  }
}

// clearPhoneLeftovers drops the open flags a phone set under its plain device
// id before it became a support lens: nothing would ever clear or re-assert
// them, so each kept its window, and the window's sessions, awake for
// store.OpenFlagTTL. An empty open-set clears exactly that one device's rows.
// Runs on every phone load; after the first it finds nothing to clear.
function clearPhoneLeftovers() {
  if (!phoneLens) return;
  fetch("/v1/windows/open-set", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ device: deviceId, deviceLabel: deviceLabel(), windowIds: [] }),
  }).then((r) => {
    if (!r.ok) console.warn("phone: clearing old open flags failed:", r.status);
  }).catch((e) => console.warn("phone: clearing old open flags failed:", e));
}

// --- base64 <-> bytes (terminal I/O travels base64 in JSON frames) ---
function b64ToBytes(b64) {
  const bin = atob(b64);
  const arr = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) arr[i] = bin.charCodeAt(i);
  return arr;
}
function bytesToB64(u8) {
  let s = "";
  for (const b of u8) s += String.fromCharCode(b);
  return btoa(s);
}

// --- workspace list ---
// Monotonic read counter. fetchWorkspaces runs from a 5s timer, from the
// firehose, and after every mutation, with no in-flight guard, so two reads
// overlap routinely and can resolve OUT OF ORDER. That was cosmetic while a
// late answer only overwrote state.windows; it stopped being cosmetic when
// callers await a read and then act on it: createWorkspace and reviveWorkspace
// attach immediately afterwards and resolve the owning host out of
// state.workspaces, while creating a workspace publishes workspace-added, whose
// firehose handler issues a second read. Without ordering, the awaited read is
// superseded and attach runs against the pre-create list, dialling the hub for
// a workspace only the owning host serves.
// The watermark is what has APPLIED, not what has been ISSUED. Gating on the
// newest issued read starves: fetchWorkspaces fires from the timer and from
// every firehose frame, so whenever latency exceeds the gap between triggers
// EVERY response is superseded before it resolves and nothing is ever applied
// — state freezes and, worse, the keep-alive never runs, so this device's
// rows age out of the daemon's TTL while the tab is genuinely open. That is
// the same window-reads-closed, sessions-archived-underneath failure the
// ordering guard exists to prevent, just reached from the other side.
//
// Against the watermark an older response still applies while nothing newer
// has landed, and is dropped the moment something newer has.
let windowsSeq = 0;
let windowsApplied = 0;
let windowsReadFailing = false;
// readWindows reads the window list, or null on a failed read. Keep the last
// list on a failed read (503 = tables unreadable, per the daemon): blanking it
// would make every window look closed and feed wrong close decisions. Same
// fallback the Mac lens used. Logged on the way INTO failure only: this runs
// every 5s and on every firehose frame, so logging each miss would bury the
// line that explains it.
async function readWindows(winr) {
  if (!winr.ok) {
    const body = await winr.text();
    if (!windowsReadFailing) console.error("window list read failed:", winr.status, body);
    windowsReadFailing = true;
    return null;
  }
  const windows = (await winr.json()) || [];
  if (windowsReadFailing) console.warn("window list read recovered");
  windowsReadFailing = false;
  return windows;
}

async function fetchWorkspaces() {
  const seq = ++windowsSeq;
  try {
    const [wr, winr] = await Promise.all([
      fetch("/v1/workspaces"),
      // device= so each window reports openHere: whether THIS lens holds it,
      // which openBy (per-login) cannot answer.
      fetch("/v1/windows?device=" + encodeURIComponent(windowDevice)),
    ]);
    // Parse BEFORE the staleness test: the bodies have to be consumed either
    // way, and the test has to sit as close to the write as possible.
    const workspaces = (await wr.json()) || [];
    const windows = await readWindows(winr);
    if (seq <= windowsApplied) return; // something newer already landed
    windowsApplied = seq;
    state.workspaces = workspaces;
    if (windows) {
      state.windows = windows;
      state.windowsKnown = true; // pagewin.js boots on the first good read, not the first try
      keepOpenFlagsAlive();
    }
  } catch (_) {
    state.workspaces = [];
  }
  resumeIfAwake();
  syncPaneTitles();
  renderList();
  if (window.ccmuxBoard) window.ccmuxBoard.refresh();
  if (window.ccmuxPage) window.ccmuxPage.onRefresh();
}

// fetchHosts loads the federation registry (hub mode). 404/empty in single-host
// mode leaves the map empty, so attach falls back to same-origin. host label →
// {id, addr, healthy, compat, ...}.
async function fetchHosts() {
  try {
    const r = await fetch("/v1/hosts");
    if (!r.ok) { state.hosts = {}; return; }
    const list = (await r.json()) || [];
    state.hosts = Object.fromEntries(list.map((h) => [h.id, h]));
  } catch (_) {
    state.hosts = {};
  }
}

// attachOrigin returns the WS origin for a workspace's terminal stream. Terminal
// bytes go DIRECT to the owning host (never relayed through the hub), so a
// workspace on another host dials wss://<host.addr>; one on the daemon serving
// this page (self), or with no known host, stays same-origin. Self matters: its
// sessions are stamped with its own id, and dialing its tailnet name from a page
// opened under another name (ccmux.dev...) cost a fresh name lookup per session
// switch, measured at 1.5s on a Mac after a few idle minutes.
function attachOrigin(ws) {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const h = ws && ws.host && state.hosts[ws.host];
  if (h && h.addr && !h.self) return `${proto}://${h.addr}`;
  return `${proto}://${location.host}`;
}

// Pane titles change while attached (the daemon re-derives them from tmux as
// programs start and stop); fold the refreshed titles into the open session's
// tabs. Pane add/close still re-attaches via the pane-added/pane-closed frames.
function syncPaneTitles() {
  // Both the list and the strip hold still while a tab is being dragged: the
  // drop index is counted over the chips on screen and applied to the list,
  // so the two must not drift apart mid-gesture. The refresh that fired
  // during the drag is not lost — the daemon's answer to the drop, or the
  // next poll, folds everything in.
  if (state.tabDrag) return;
  const ws = state.workspaces.find((w) => w.id === state.wsId);
  if (!ws || !ws.panes) return;
  let changed = syncPaneOrder(ws), shellChanged = false;
  for (const p of state.panes) {
    const q = ws.panes.find((x) => x.id === p.id);
    if (!q) continue;
    if (q.title !== p.title) { p.title = q.title; changed = true; }
    // atShell/harness arrive late for a fresh pane (the attach hello races
    // tmux's first command signal), so the harness bar re-derives from every
    // registry refresh — the firehose fires one for exactly this change.
    if (!!q.atShell !== !!p.atShell || !!q.dormant !== !!p.dormant ||
        !!q.devServer !== !!p.devServer || (q.harness || "") !== (p.harness || "") ||
        (q.agent || "") !== (p.agent || "") || (q.agentVersion || "") !== (p.agentVersion || "") ||
        (q.claudeUpdate || "") !== (p.claudeUpdate || "")) {
      p.atShell = q.atShell;
      p.dormant = q.dormant;
      p.devServer = q.devServer;
      p.harness = q.harness;
      p.agent = q.agent;
      p.agentVersion = q.agentVersion;
      p.claudeUpdate = q.claudeUpdate;
      shellChanged = true;
    }
  }
  if (changed || shellChanged) renderTabs(); // dormant styling rides the tabs
  if (shellChanged) updateHarnessBar();
}

// pageWindow is the window THIS browser window shows (pagewin.js), "" for none.
const pageWindow = () => (window.ccmuxPage ? window.ccmuxPage.current() : "");
// carriesBoard: only the current window carries the Attention row, or every
// open one while this browser window shows none.
const carriesBoard = (win, here) => !!win && (!here || win.id === here);
// shownElsewhere: win is another open window than the one this browser window
// shows. Its header and rows are dimmed and shorter, so the current window
// stands out; a row that needs you stays bright (style.css .elsewhere).
const shownElsewhere = (win, here) => !!here && !!win && win.id !== here;
// hdrClass is an open window's header classes: the current one, or one shown elsewhere.
const hdrClass = (win, here) =>
  "group-hdr" + (win && win.id === here ? " current" : shownElsewhere(win, here) ? " elsewhere" : "");
// rowClass is a session row's classes: "ws" plus each flag that is set, by name.
const rowClass = (flags) => ["ws", ...Object.keys(flags).filter((k) => flags[k])].join(" ");

// renderList renders the SHARED arrangement (v2): windows you have OPEN as
// sections, windows you have closed as one-line rows you can open, and
// ungrouped sessions under AVAILABLE. The arrangement is the same for
// everyone; open/closed is yours. The window THIS browser window shows
// (pagewin.js) comes first and alone carries the Attention row, as on the Mac.
function renderList() {
  const ul = $("ws-list");
  ul.innerHTML = "";
  const grouped = withEmptyWindows(groupedWorkspaces());
  const winByName = new Map(state.windows.map((w) => [w.name.toLowerCase(), w]));
  // openHere, not open: a window another of your lenses holds is not open HERE,
  // and hiding it left it unreachable from this lens entirely. ?? open so an
  // older daemon, which omits the field, keeps the previous per-login behavior
  // instead of rendering every open window as closed and dropping its rows.
  const closed = state.windows.filter((w) => !openHereOf(w));
  const here = pageWindow();
  const isHere = (group) => !!here && (winByName.get(group.toLowerCase()) || {}).id === here;
  grouped.sort(([a], [b]) => Number(isHere(b)) - Number(isHere(a))); // stable: the rest keep their order

  for (const [group, list] of grouped) {
    const win = winByName.get(group.toLowerCase()); // none for AVAILABLE ("")
    if (group) {
      if (win && !openHereOf(win)) continue; // rendered below as a closed-window row
      ul.appendChild(groupHeader(group, win, here));
      if (carriesBoard(win, here)) {
        ul.appendChild(boardRow(group));
        if (window.ccmuxTasks) ul.appendChild(window.ccmuxTasks.row(win)); // the window's own task queue
      }
    } else if (grouped.length > 1 || closed.length) {
      // Header only when there is something to separate from: a deployment
      // where nothing is grouped keeps its plain flat list.
      const h = document.createElement("li");
      h.className = "group-hdr";
      h.innerHTML = "<span>AVAILABLE</span>";
      ul.appendChild(h);
    }
    for (const ws of list) ul.appendChild(wsRow(ws, shownElsewhere(win, here)));
  }
  appendClosedWindows(ul, closed);
}

// groupHeader is an open window's section header. Another window's header
// switches this browser window to it; the current one is marked.
function groupHeader(group, win, here) {
  const h = document.createElement("li");
  h.className = hdrClass(win, here);
  h.innerHTML = `<span>${esc(group.toUpperCase())}</span>` +
    (win && !phoneLens ? `<button class="grp-close" title="Close window">–</button>` : "") +
    `<button class="grp-msgs" title="Peer messages in ${esc(group)}">💬</button>` +
    (win ? `<button class="grp-agents" title="Agents in ${esc(group)}">⚙</button>` : "");
  h.querySelector(".grp-msgs").onclick = (e) => {
    e.stopPropagation();
    window.ccmuxPeers.open(group);
  };
  if (!win) return h;
  if (!phoneLens) h.querySelector(".grp-close").onclick = (e) => {
    e.stopPropagation();
    closeWindow(win);
  };
  // Agents belong to the window (the project), not to one session:
  // the header is where they are added, woken and put to sleep.
  h.querySelector(".grp-agents").onclick = (e) => {
    e.stopPropagation();
    openWindowMenu(win, e.clientX, e.clientY);
  };
  h.oncontextmenu = (e) => {
    e.preventDefault();
    openWindowMenu(win, e.clientX, e.clientY);
  };
  // Another window's header: to the browser window already showing it, when
  // there is one (pagewin.js handOff), else into this one.
  if (window.ccmuxPage && win.id !== here) h.onclick = async () => {
    if (!(await window.ccmuxPage.handOff(win.id, ""))) window.ccmuxPage.enter(win.id, "");
  };
  return h;
}

// appendClosedWindows lists the windows this lens has closed, one row each.
function appendClosedWindows(ul, closed) {
  if (!closed.length) return;
  const h = document.createElement("li");
  h.className = "group-hdr";
  h.innerHTML = "<span>CLOSED WINDOWS</span>";
  ul.appendChild(h);
  for (const win of closed) {
    const li = document.createElement("li");
    li.className = "ws closed-window";
    const n = (win.workspaceIds || []).length;
    li.innerHTML = `<div class="ws-row">` +
      `<span class="dot cold"></span>` +
      `<span class="name">${esc(win.name)}</span>` +
      `<span class="owner-tag">${n} repo${n === 1 ? "" : "s"}</span>` +
      `<button class="more" title="Open window">▸</button>` +
      `</div>`;
    li.onclick = () => (window.ccmuxPage ? window.ccmuxPage.openClosed(win, "") : openWindow(win));
    li.oncontextmenu = (e) => {
      e.preventDefault();
      if (window.ccmuxPage) openClosedWindowMenu(win, e.clientX, e.clientY);
    };
    ul.appendChild(li);
  }
}

// boardRow is an open window's "Attention" entry: the count of its panes
// waiting on a human, and the way into the attention board (board.js). Same
// row, same count rule as the Mac sidebar's.
function boardRow(group) {
  const n = window.ccmuxBoard ? window.ccmuxBoard.count(group) : 0;
  const li = document.createElement("li");
  li.className = "ws board-row" + (state.board === group ? " active" : "") + (n ? " waiting" : "");
  li.innerHTML = `<div class="ws-row"><span class="board-icon">◎</span>` +
    `<span class="name">Attention</span>` + (n ? `<span class="board-count">${n}</span>` : "") + `</div>`;
  li.onclick = () => window.ccmuxBoard && window.ccmuxBoard.open(group);
  return li;
}

// openWorkspace is every explicit "show this session" (sidebar, menus, a new
// or revived session, a deep link): it leaves the attention board first, which
// attach alone does not, because the board's active tile attaches through it
// too. Re-attaches of the session on show (tabs, take over, a reconnect) stay
// on attach. The Mac app's WorkspaceWindowController.display is the same rule.
// openWorkspace is a deliberate open: its row, a menu, a task, a
// notification tap. A sleeping session is woken first.
function openWorkspace(wsId, wantPane) {
  if (window.ccmuxRoute.sessionHome(wsById(wsId))) return openAgentSession(wsId);
  closeSessionlessViews();
  if (isAsleep(wsId)) return reviveWorkspace(wsId, wantPane);
  return attach(wsId, wantPane);
}

// openAgentSession shows a role agent that runs as a session, not in a
// terminal: its home has nothing to attach or wake here, and its
// conversation is on the thread page. An agent with no session yet (it
// never started here, or its last one was ended) gets one from its next
// message.
async function openAgentSession(wsId) {
  const name = (wsById(wsId) || {}).name || "This agent";
  try {
    const r = await fetch("/v1/sessions");
    if (!r.ok) throw new Error(await refusalOf(r));
    const href = window.ccmuxRoute.agentSessionHref((await r.json()).sessions, wsId);
    if (href) location.href = href;
    else alert(`${name} has no session yet. Message it, or wake it from the window's menu, and it starts.`);
  } catch (e) {
    alert(`Could not open ${name}: ${e.message}`);
  }
}

// showWorkspace is the page putting back what it showed: a reload (also the
// automatic one after a deploy), a relaunch, a window switch, a phone launch.
// It never wakes anything: a sleeping session shows asleep (attach), so a
// closed session stays closed and a reopened window wakes only what the
// daemon marked awake at its close.
function showWorkspace(wsId, wantPane) {
  closeSessionlessViews();
  return attach(wsId, wantPane);
}

// closeSessionlessViews takes down the attention board and the task queue,
// which hold no session, before one is shown.
function closeSessionlessViews() {
  if (window.ccmuxBoard) window.ccmuxBoard.close();
  if (window.ccmuxTasks) window.ccmuxTasks.close();
}

// leaveWorkspace is for a view that holds no session (the attention board,
// the task queue): drop whatever was attached, without the drawer-opening
// detachIfCurrent does for a closed session.
function leaveWorkspace() {
  if (state.conn) { const c = state.conn; state.conn = null; c.close(); }
  state.wsId = null;
  state.paneId = null;
  state.panes = [];
  window.ccmuxAgentChat.hide();
  renderTabs();
}

// groupedWorkspaces buckets by the shared window name: named windows
// alphabetically, the ungrouped bucket ("") last; inside a bucket, repos by
// name then agents by name.
// withEmptyWindows adds a section for each window open here that has no
// session: its Tasks row is the only way to its queue, and the daemon keeps
// such a window while its queue holds a task. Before AVAILABLE, which stays last.
function withEmptyWindows(grouped) {
  const have = new Set(grouped.map(([g]) => g.toLowerCase()));
  const empty = state.windows.filter((w) => openHereOf(w) && !have.has(w.name.toLowerCase())).map((w) => [w.name, []]);
  const loose = grouped.findIndex(([g]) => g === "");
  if (loose < 0) return grouped.concat(empty);
  return grouped.slice(0, loose).concat(empty, grouped.slice(loose));
}

function groupedWorkspaces() {
  const byGroup = new Map();
  for (const ws of state.workspaces) {
    const g = ws.group || "";
    if (!byGroup.has(g)) byGroup.set(g, []);
    byGroup.get(g).push(ws);
  }
  for (const list of byGroup.values()) {
    // Repos first by name, then the window's agents by name: an agent
    // session is a member of the project, not a peer of its repos.
    list.sort((a, b) => (Number(isAgentWs(a)) - Number(isAgentWs(b))) ||
      (a.name || "").localeCompare(b.name || "", undefined, { sensitivity: "base" }));
  }
  return [...byGroup.entries()].sort(([a], [b]) =>
    a === "" ? 1 : b === "" ? -1 : a.localeCompare(b));
}

// ownerLabel says whose session an ungrouped row is: "patric". Empty for
// grouped rows (the window is shared; whose host it runs on is a detail).
function ownerLabel(ws) {
  if (ws.group || !ws.owner) return "";
  return (ws.owner || "").split("@")[0];
}

const wsById = (id) => state.workspaces.find((w) => w.id === id);

// openWindow marks the shared window open for this login and wakes the
// members the daemon has marked awake from the window's last close that are
// asleep now (windowwake.js) — opening a sleeping window is how it comes back
// everywhere. A window with no marks (closed before marks existed, or from
// the Mac app) wakes nothing. True when the window is now open here (a member
// that would not wake still is).
async function openWindow(win) {
  let r;
  try {
    r = await fetch(`/v1/windows/${win.id}/open?device=${encodeURIComponent(windowDevice)}&deviceLabel=${encodeURIComponent(deviceLabel())}`, { method: "POST" });
  } catch (e) {
    alert("open failed: " + e.message);
    return false;
  }
  if (!r.ok) { alert("open failed: " + (await r.text().catch(() => r.status))); return false; }
  // The open landed: from here a failure is a session that did not wake,
  // never a failed open.
  await wakeMembers(win, await openAnswer(r, win));
  await fetchWorkspaces();
  return true;
}

// openAnswer reads what an open says to wake. An answer the lens cannot read
// wakes nothing and says so: the marks stay on the daemon, so nothing is
// lost, but the sessions stay asleep until woken from the sidebar.
async function openAnswer(r, win) {
  try {
    return await r.json();
  } catch (e) {
    console.error("window open: unreadable answer, waking nothing:", win.id, e);
    alert(`${win.name || "The window"} opened, but ccmux could not read which sessions to wake. Wake them from the sidebar.`);
    return { wake: [] };
  }
}

// wakeMembers wakes what the open's answer names (windowwake.js), each on
// its own, and says once which would not wake.
async function wakeMembers(win, answer) {
  const failed = [];
  for (const wsId of window.ccmuxWindowWake.toWake(answer, win.workspaceIds, wsById)) {
    const name = (wsById(wsId) || {}).name || wsId;
    try {
      const rr = await fetch(`/v1/workspaces/${wsId}/revive`, { method: "POST" });
      if (!rr.ok) failed.push(`${name}: ${await rr.text()}`);
    } catch (e) {
      failed.push(`${name}: ${e.message}`);
    }
  }
  if (failed.length) alert("The window opened, but these sessions did not wake:\n" + failed.join("\n"));
}

// closeWindow clears this login's open flag; when that was the LAST opener,
// the window goes to sleep — its members archive (force: nobody has it open,
// which is the model's own permission).
async function closeWindow(win) {
  try {
    // The sessions awake now, so the next open wakes only those (windowwake.js).
    const awake = window.ccmuxWindowWake.awakeAt(win.workspaceIds, wsById);
    const r = await fetch(`/v1/windows/${win.id}/close?device=${encodeURIComponent(windowDevice)}`, {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ awake }),
    });
    if (!r.ok) { alert("close failed: " + (await r.text())); return; }
    const out = await r.json();
    if (out.last) {
      for (const wsId of out.members || []) {
        detachIfCurrent(wsId);
        const ar = await fetch(`/v1/workspaces/${wsId}/archive?force=1`, { method: "POST" });
        // A member that failed to sleep keeps running — say so, or it reads
        // as a ghost later.
        if (!ar.ok) alert(`could not sleep ${wsId}: ` + (await ar.text()));
      }
    }
  } catch (e) {
    alert("close failed: " + e.message);
    return;
  }
  if (window.ccmuxPage) window.ccmuxPage.onClosed(win.id);
  fetchWorkspaces();
}

// isAgentWs: the session is an agent instance (the daemon stamps the base
// name on the workspace). Such a row is one line: no git dashboard (its
// folder is not a repo), an "agent" tag instead, sorted after the repos.
// The Mac sidebar's AgentWorkspaceRow follows the same rule but marks the
// row with a spark icon instead of the tag, on purpose: the tag reads well
// here and not there.
function isAgentWs(ws) {
  return !!ws.agent;
}

// wsBadges is the right-hand column of a session row: what it is (agent,
// or whose it is), whether it is working, and its git state or sleep.
function wsBadges(ws, agent, cold, running, label) {
  let tag = "";
  if (agent) tag = `<span class="owner-tag agent-tag">agent</span>`;
  else if (label) tag = `<span class="owner-tag">${esc(label)}</span>`;
  let trailing = "";
  if (cold) trailing = `<span class="cold-tag">zzz</span>`;
  else if (!agent) trailing = gitBadges(ws.git);
  return tag + (running ? `<span class="bolt">⚡</span>` : "") + trailing;
}

// clickRow is a click on a session row. One of another window's sessions goes
// to the browser window already showing that window (pagewin.js handOff);
// anything else opens here.
async function clickRow(id) {
  if (window.ccmuxPage && await window.ccmuxPage.handOff("", id)) return;
  openRow(id);
}

// openRow shows a session the way its row does (openWorkspace wakes a
// sleeping one). pagewin.js calls it for a hand-off.
function openRow(id) {
  return openWorkspace(id, null);
}

// wsRow is one session's row; elsewhere marks a session of another open window.
function wsRow(ws, elsewhere) {
  const active = ws.id === state.wsId && !state.board;
  const agent = isAgentWs(ws);
  // Suppress the flash on the workspace you're already watching (mirrors the
  // native "clear on watch"); other rows flash live from the firehose and stop
  // when the daemon says the workspace was looked at somewhere (attention idle).
  const att = active ? "" : wsAttention(ws);
  const open = !!state.gitOpen[ws.id];
  const running = (ws.panes || []).some((p) => p.attention === "running");
  const cold = ws.status === "cold";
  const label = ownerLabel(ws);
  const li = document.createElement("li");
  li.className = rowClass({ active, ["att-" + att]: !!att, cold, elsewhere });
  li.innerHTML =
    `<div class="ws-row">` +
    (agent ? `<span class="exp none"></span>` : `<span class="exp${open ? " open" : " closed"}"></span>`) +
    `<span class="dot ${esc(ws.status)}"></span>` +
    `<span class="name">${esc(ws.name || ws.repoPath)}</span>` +
    wsBadges(ws, agent, cold, running, label) +
    `<button class="more" title="Session menu">⋯</button>` +
    `</div>` +
    (open && !agent ? gitDetail(ws) : "");
  li.onclick = () => clickRow(ws.id);
  if (!agent) li.querySelector(".exp").onclick = (e) => {
    e.stopPropagation();
    state.gitOpen[ws.id] = !open;
    renderList();
  };
  li.oncontextmenu = (e) => {
    e.preventDefault();
    e.stopPropagation();
    openWsMenu(ws, e.clientX, e.clientY);
  };
  li.querySelector(".more").onclick = (e) => {
    e.stopPropagation();
    const r = e.target.getBoundingClientRect();
    openWsMenu(ws, r.left, r.bottom + 2);
  };
  return li;
}

// --- workspace context menu: mirrors the Mac hosted row's menu. Right-click
// on desktop; the row's ⋯ covers touch (iOS fires no contextmenu event). ---
function openWsMenu(ws, x, y) {
  const menu = $("ctx-menu");
  menu.innerHTML = "";
  // Federation: the only place a workspace's host is surfaced — a read-only line.
  if (ws.host) {
    const line = document.createElement("div");
    line.className = "host-line";
    line.textContent = "⬡ " + ws.host;
    menu.appendChild(line);
  }
  const add = (label, fn, cls) => {
    const b = document.createElement("button");
    b.textContent = label;
    if (cls) b.className = cls;
    b.onclick = () => { closeWsMenu(); fn(); };
    menu.appendChild(b);
  };
  const sep = () => menu.appendChild(Object.assign(document.createElement("div"), { className: "sep" }));

  add("Open in New Tab", () => window.open(`/?ws=${ws.id}`, "_blank"));
  sep();
  // Windows are SHARED (v2): placing, moving, and removing change the one
  // arrangement everyone sees.
  add(ws.group ? "Move to Window…" : "Add to Window…", () => moveToWindow(ws));
  if (ws.group) add("Remove from Window (for everyone)", () => putGroup(ws.id, ""));
  // An agent's session owns no repo; its description says what it does.
  if (!isAgentWs(ws)) add("What It Owns…", () => window.ccmuxOwns.open(ws));
  sep();
  if (ws.status === "cold") add("Revive", () => openWorkspace(ws.id, null));
  else addLiveEntries(add, sep, ws);
  add("Remove Session…", () => removeSession(ws), "danger");

  showMenuAt(menu, x, y);
}

// addLiveEntries adds an awake session's rows to its menu: hostnames, the dev
// server, and Close Session.
function addLiveEntries(add, sep, ws) {
  add("Hostnames…", () => openHostnamesModal(ws));
  const hostnames = ws.hostnames || [];
  if (hostnames.length) {
    // "Running" is the pane existing AND the port answering — same rule as
    // the Mac lens's ▶/■. A crashed server's pane survives at a shell, so
    // both entries show there (Start re-types the command into that pane,
    // daemon-side). A server started by hand keeps Start visible too.
    const paneRunning = (ws.panes || []).some((p) => p.devServer);
    const listening = hostnames.some((h) => h.listening);
    if (!(paneRunning && listening)) add("Start Dev Server", () => setDevServer(ws.id, true));
    if (paneRunning) add("Stop Dev Server", () => setDevServer(ws.id, false));
  }
  for (const h of hostnames.filter((h) => h.url)) {
    add(`${h.listening ? "●" : "○"} ${h.name} : ${h.port}${hostnameWhere(h)}`, () => window.open(h.url, "_blank"));
  }
  sep();
  add("Close Session", () => closeSession(ws.id));
}

// showMenuAt shows a context menu first so it has a size, then clamps it
// into the viewport.
function showMenuAt(menu, x, y) {
  menu.classList.remove("hidden");
  const r = menu.getBoundingClientRect();
  menu.style.left = Math.max(4, Math.min(x, window.innerWidth - r.width - 8)) + "px";
  menu.style.top = Math.max(4, Math.min(y, window.innerHeight - r.height - 8)) + "px";
}

// openWindowMenu is the window header's menu: one row per base agent —
// running → open its session or put it to sleep; asleep or not yet added →
// start it here with an optional first message. Rows fill in once fetched,
// bound to THIS menu's window.
function openWindowMenu(win, x, y) {
  const menu = $("ctx-menu");
  menu.innerHTML = "";
  if (window.ccmuxPage && win.id !== pageWindow()) {
    menuButton(menu, "Open in new window", () => window.ccmuxPage.openElsewhere(win));
  }
  const line = document.createElement("div");
  line.className = "host-line";
  line.textContent = "⚙ agents in " + win.name;
  menu.appendChild(line);
  const slot = document.createElement("div");
  slot.className = "agent-slot";
  menu.appendChild(slot);
  menu.dataset.win = win.id;
  appendWindowAgentEntries(menu, slot, win);
  showMenuAt(menu, x, y);
}

async function appendWindowAgentEntries(menu, slot, win) {
  const list = await fetchWindowAgents(win.id);
  if (menu.dataset.win !== win.id || !slot.isConnected) return;
  const note = (text) => slot.appendChild(Object.assign(document.createElement("div"), { className: "host-line", textContent: text }));
  if (agentCatalog.error) { note("agents: " + agentCatalog.error); return; }
  if (!list.length) { note("no agents defined — Settings › Agents"); return; }
  const add = (label, fn) => {
    const b = document.createElement("button");
    b.textContent = label;
    b.onclick = () => { closeWsMenu(); fn(); };
    slot.appendChild(b);
  };
  for (const a of list) addAgentEntries(add, a, win);
}

// addAgentEntries adds one agent's rows to the window menu: open and sleep
// for a running one, wake or add for the rest.
function addAgentEntries(add, a, win) {
  // No stand-in glyph for an icon-less base (same rule as the session name).
  const title = a.icon ? `${a.icon} ${a.name}` : a.name;
  if (a.state === "running") {
    add(`● ${title} — open`, () => openWorkspace(a.workspace, a.pane));
    add(`■ ${title} — sleep`, () => sleepAgent(win.id, a.name));
    return;
  }
  const mark = a.state === "asleep" ? "○" : "+";
  const verb = a.state === "asleep" ? "wake…" : "add…";
  add(`${mark} ${title} — ${verb}` + (a.drift ? " ↻" : ""), async () => {
    const text = prompt(`Message ${a.name} (empty = just start it):`, "");
    if (text === null) return;
    await wakeAndOpen(win.id, a.name, text.trim());
  });
}

async function sleepAgent(winId, name) {
  try {
    const r = await fetch(`/v1/windows/${winId}/agents/${encodeURIComponent(name)}`, { method: "DELETE" });
    if (!r.ok) alert(`sleep ${name}: ` + (await r.text()));
  } catch (e) {
    alert(`sleep ${name}: ` + e.message);
  }
  agentCatalog.at = 0;
}

function closeWsMenu() { $("ctx-menu").classList.add("hidden"); }

// menuButton adds one entry to a context menu; picking it closes the menu.
function menuButton(menu, label, fn) {
  const b = document.createElement("button");
  b.textContent = label;
  b.onclick = () => { closeWsMenu(); fn(); };
  menu.appendChild(b);
}

// openClosedWindowMenu is a closed window row's right-click: where to open it.
function openClosedWindowMenu(win, x, y) {
  const menu = $("ctx-menu");
  menu.innerHTML = "";
  menuButton(menu, "Open here", () => window.ccmuxPage.openClosed(win, "here"));
  menuButton(menu, "Open in new window", () => window.ccmuxPage.openClosed(win, "new"));
  showMenuAt(menu, x, y);
}

async function setDevServer(id, start) {
  const r = await fetch(`/v1/workspaces/${id}/dev-server`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ action: start ? "start" : "stop" }),
  });
  if (!r.ok) alert("dev server: " + (await r.text()));
  fetchWorkspaces();
}

// hostnameWhere: suffix after "name : port" for a name that routes elsewhere
// or cannot answer. "→ 5174": the server moved and the name follows it.
// "held by X": another workspace's pane has this port, so the daemon refuses
// the route. Same text as the Mac lens's hostnameWhere.
function hostnameWhere(h) {
  if (h.livePort) return ` → ${h.livePort}`;
  if (h.heldBy) return ` (held by ${h.heldBy})`;
  return "";
}

// Hostnames sheet. Rows are {name, port}: the port is whatever the app binds
// on its own (its script's -p, its config, the flag the user types). The
// daemon watches what the workspace's panes listen on and routes each name
// there, so the sheet shows that live list with a one-click "map". The dev
// command field is prefilled with the repo-detected command; saving it
// unchanged sends "" so it keeps following the repo.
let hostnamesWsId = null;
let hostnamesDetectedCmd = "";
let hostnamesLiveTimer = null;
let hostnamesLiveFailed = false;

async function openHostnamesModal(ws) {
  hostnamesWsId = ws.id;
  hostnamesDetectedCmd = "";
  $("hostnames-title").textContent = `Hostnames — ${ws.name}`;
  $("hostnames-error").classList.add("hidden");
  $("hostnames-cmd-hint").classList.add("hidden");
  $("hostnames-cmd-caption").classList.add("hidden");
  $("hostnames-rows").innerHTML = "";
  let s = null;
  try { s = await fetchPortSuggestions(ws.id); } catch (e) {
    console.warn("hostnames: port-suggestions read failed", e);
  }
  prefillHostnameRows(ws, s);
  applyDevCommandPrefill(ws, s);
  // After the rows: "mapped" vs "map" is judged against them. A failed read
  // says so rather than claiming nothing listens.
  renderHostnamesLive(s ? s.listening || [] : null, ws.name);
  $("hostnames-modal").classList.remove("hidden");
  startHostnamesLivePoll(ws);
}

// prefillHostnameRows: stored mappings, else the repo-detected ones, else one
// empty editor row.
function prefillHostnameRows(ws, s) {
  let mappings = (ws.hostnames || []).map((h) => ({ name: h.name, port: h.port }));
  if (!mappings.length && s) {
    mappings = (s.suggestions || []).map((x) => ({ name: x.name, port: x.port }));
  }
  if (!mappings.length) mappings = [{ name: "", port: "" }];
  for (const m of mappings) $("hostnames-rows").appendChild(hostnameRow(m.name, m.port));
}

// applyDevCommandPrefill: the stored override, else the detected command as
// real text with a caption saying where it came from. A stored override
// whose repo-detected counterpart moved on gets a one-click way back.
function applyDevCommandPrefill(ws, s) {
  let cmd = ws.devCommand || "";
  if (!s) {
    $("hostnames-cmd").value = cmd;
    return;
  }
  hostnamesDetectedCmd = s.detectedCommand || "";
  if (!cmd && s.devCommand) {
    cmd = s.devCommand;
    $("hostnames-cmd-caption").textContent = `detected from ${s.devCommandSource || "the repo"} — edit to override`;
    $("hostnames-cmd-caption").classList.remove("hidden");
  }
  if (ws.devCommand && s.detectedCommand && s.detectedCommand !== ws.devCommand) {
    $("hostnames-cmd-detected").textContent = `repo now detects: ${s.detectedCommand}`;
    $("hostnames-cmd-use").onclick = () => {
      $("hostnames-cmd").value = s.detectedCommand;
      $("hostnames-cmd-hint").classList.add("hidden");
    };
    $("hostnames-cmd-hint").classList.remove("hidden");
  }
  $("hostnames-cmd").value = cmd;
}

// startHostnamesLivePoll keeps "Listening now" fresh while the sheet is
// open, from the cheap listeners route — a server started in a pane shows
// up within a few seconds (one daemon scan tick plus one poll).
function startHostnamesLivePoll(ws) {
  clearInterval(hostnamesLiveTimer);
  hostnamesLiveFailed = false;
  hostnamesLiveTimer = setInterval(async () => {
    if ($("hostnames-modal").classList.contains("hidden") || hostnamesWsId !== ws.id) {
      clearInterval(hostnamesLiveTimer);
      return;
    }
    renderHostnamesLive(await fetchListeners(ws), ws.name);
  }, 2000);
}

async function fetchPortSuggestions(wsId) {
  const r = await fetch(`/v1/workspaces/${wsId}/port-suggestions`);
  if (!r.ok) throw new Error("HTTP " + r.status);
  return r.json();
}

// fetchListeners: the live list, or null when the read failed. Logs once on
// the way into failure (the poll would otherwise repeat it every 2 s), same
// rule as the Mac sheet's pollListening.
async function fetchListeners(ws) {
  try {
    const r = await fetch(`/v1/workspaces/${ws.id}/listeners`);
    if (!r.ok) throw new Error("HTTP " + r.status);
    hostnamesLiveFailed = false;
    return (await r.json()).listening || [];
  } catch (e) {
    if (!hostnamesLiveFailed) console.warn(`hostnames: listener poll failed for ${ws.name}`, e);
    hostnamesLiveFailed = true;
    return null;
  }
}

// renderHostnamesLive lists the workspace's current listeners: a port a row
// already names says "mapped", any other gets a "map" button that adds a row.
// listening=null means the read FAILED — that is not "nothing listens", and
// the runbook sends people here to diagnose a 502, so say which it is.
function renderHostnamesLive(listening, wsName) {
  const ul = $("hostnames-live");
  ul.innerHTML = "";
  if (!listening) return liveEmpty(ul, "could not read listeners from the daemon — is it up?");
  if (!listening.length) return liveEmpty(ul, "nothing yet — start the dev server in a pane and it shows up here");
  const mapped = new Set([...$("hostnames-rows").children].map((li) => li.querySelector(".hn-port").value.trim()));
  for (const l of listening) {
    const li = document.createElement("li");
    const port = document.createElement("span");
    port.className = "hn-live-port";
    port.textContent = String(l.port);
    const proc = document.createElement("span");
    proc.textContent = l.process || "";
    li.append(port, proc);
    if (mapped.has(String(l.port))) {
      const done = document.createElement("span");
      done.textContent = "mapped";
      li.appendChild(done);
    } else {
      const map = document.createElement("button");
      map.className = "hn-add";
      map.type = "button";
      map.textContent = "map";
      map.onclick = () => {
        const rows = $("hostnames-rows");
        // Replace a lone empty editor row rather than stacking under it.
        const only = rows.children.length === 1 ? rows.children[0] : null;
        if (only && !only.querySelector(".hn-name").value && !only.querySelector(".hn-port").value) only.remove();
        rows.appendChild(hostnameRow(suggestHostnameLabel(wsName, rows.children.length), l.port));
        renderHostnamesLive(listening, wsName);
      };
      li.appendChild(map);
    }
    ul.appendChild(li);
  }
}

function liveEmpty(ul, text) {
  const li = document.createElement("li");
  li.className = "hn-live-empty";
  li.textContent = text;
  ul.appendChild(li);
}

// suggestHostnameLabel: the workspace slug for the first row, slug-2… after.
function suggestHostnameLabel(wsName, existing) {
  const slug = (wsName || "app").toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "") || "app";
  return existing ? `${slug}-${existing + 1}` : slug;
}

function hostnameRow(name, port) {
  const li = document.createElement("li");
  const n = document.createElement("input");
  n.className = "setting-input hn-name";
  n.placeholder = "name";
  n.spellcheck = false;
  n.value = name || "";
  const p = document.createElement("input");
  p.className = "setting-input hn-port";
  p.placeholder = "port";
  p.title = "The port the app binds on its own";
  p.inputMode = "numeric";
  p.value = port || "";
  const del = document.createElement("button");
  del.className = "hn-del";
  del.title = "Remove mapping";
  del.textContent = "×";
  del.onclick = () => li.remove();
  li.append(n, p, del);
  return li;
}

async function saveHostnames() {
  const err = $("hostnames-error");
  const hostnames = [];
  for (const li of $("hostnames-rows").children) {
    const name = li.querySelector(".hn-name").value.trim();
    const portText = li.querySelector(".hn-port").value.trim();
    if (!name && !portText) continue; // half-empty editor row, not a mapping
    const port = /^\d+$/.test(portText) ? parseInt(portText, 10) : 0;
    if (port < 1 || port > 65535) {
      err.textContent = `${name || "row"}: port must be 1–65535 — the port the app binds itself`;
      err.classList.remove("hidden");
      return;
    }
    hostnames.push({ name, port });
  }
  // The prefilled detected command, saved untouched, stays "" on the daemon
  // so it keeps following the repo. Same rule as the Mac sheet.
  let devCommand = $("hostnames-cmd").value.trim();
  if (devCommand === hostnamesDetectedCmd) devCommand = "";
  const body = { hostnames, devCommand };
  const r = await fetch(`/v1/workspaces/${hostnamesWsId}/hostnames`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!r.ok) {
    let msg = "HTTP " + r.status;
    try { msg = (await r.json()).error || msg; } catch (_) {}
    err.textContent = msg;
    err.classList.remove("hidden");
    return;
  }
  $("hostnames-modal").classList.add("hidden");
  fetchWorkspaces();
}

// reviveWorkspace wakes a sleeping session, then attaches it. A wake that
// fails says so, in the daemon's words, and leaves the session shown asleep:
// never a retry loop.
async function reviveWorkspace(id, wantPane) {
  const name = (wsById(id) || {}).name || id;
  try {
    const r = await fetch(`/v1/workspaces/${id}/revive`, { method: "POST" });
    if (!r.ok) throw new Error(await refusalOf(r));
  } catch (e) {
    alert(`Could not wake ${name}: ${e.message}`);
    return attach(id, wantPane);
  }
  await fetchWorkspaces();
  // Already attached when it was on screen asleep: the read above resumed it.
  if (state.wsId === id && !state.asleepWait) return;
  return attach(id, wantPane);
}

// refusalOf is the reason in a refused answer: the daemon sends
// {"error": "..."}, anything else is shown as it came.
async function refusalOf(r) {
  const body = await r.text();
  try { return JSON.parse(body).error || body; } catch (_) { return body; }
}

// putGroup writes YOUR view row: which of your windows the session sits in.
// "" puts it away (back to Available). Nobody else's arrangement changes.
async function putGroup(id, group) {
  // try/catch: on a phone a network failure otherwise dies as an unhandled
  // rejection — no alert, no console, a tap that just does nothing.
  let r;
  try {
    r = await fetch(`/v1/workspaces/${id}/group`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ group }),
    });
  } catch (e) {
    alert("window change failed: " + e.message);
    return;
  }
  if (!r.ok) { alert("window change failed: " + (await r.text())); return; }
  fetchWorkspaces();
}

function moveToWindow(ws) {
  const names = state.windows.map((w) => w.name).sort();
  const hint = names.length ? `\n\nWindows: ${names.join(", ")}` : "";
  const g = prompt("Window name (shared — moves it for everyone):" + hint, ws.group || "");
  if (g === null) return;
  putGroup(ws.id, g.trim());
}

// guarded409 runs an archive/remove-style call; when the daemon's guard
// refuses — 409 (someone else's session, or someone still has it in a window)
// or 503 (the guard could not read its evidence) — it asks, and retries with
// force=1 on yes. Network failures alert instead of dying as unhandled
// rejections (a phone has no console).
async function guarded409(makeReq, label) {
  let r;
  try {
    r = await makeReq(false);
  } catch (e) {
    alert(label + " failed: " + e.message);
    return null;
  }
  if (r.status === 409 || r.status === 503) {
    let msg = "HTTP " + r.status;
    try { msg = (await r.json()).error || msg; } catch (_) {}
    if (confirm(msg.replace(/ — (pass force=1|retry).*$/, "") + `\n\n${label} anyway?`)) {
      let rf;
      try {
        rf = await makeReq(true);
      } catch (e) {
        alert(label + " failed: " + e.message);
        return null;
      }
      if (!rf.ok) { alert(label + " failed: " + (await rf.text())); return null; }
      return rf;
    }
    return null;
  }
  if (!r.ok) { alert(label + " failed: " + (await r.text())); return null; }
  return r;
}

async function closeSession(id) {
  const r = await guarded409(
    (force) => fetch(`/v1/workspaces/${id}/archive` + (force ? "?force=1" : ""), { method: "POST" }),
    "Close");
  if (!r) return;
  detachIfCurrent(id);
  fetchWorkspaces();
}

async function removeSession(ws) {
  const sure = confirm(
    `Remove “${ws.name}”?\n\nThis kills the session and permanently deletes its ` +
    `panes, layout, hostnames, and dev command. Use “Close session” instead to ` +
    `keep them for a later revive.`);
  if (!sure) return;
  const r = await guarded409(
    (force) => fetch(`/v1/workspaces/${ws.id}` + (force ? "?force=1" : ""), { method: "DELETE" }),
    "Remove");
  if (!r) return;
  detachIfCurrent(ws.id);
  fetchWorkspaces();
}

// detachIfCurrent drops the attach when the workspace we're watching goes away
// (closed/removed) — back to the empty state, so re-surface the session list.
function detachIfCurrent(id) {
  if (state.wsId !== id) return;
  if (window.ccmuxScratch) window.ccmuxScratch.hide();
  if (state.conn) { state.conn.close(); state.conn = null; }
  state.wsId = null;
  state.panes = [];
  if (window.ccmuxPage) window.ccmuxPage.syncAddress();
  renderTabs();
  $("empty").style.display = "";
  openDrawer();
}

// --- git dashboard (daemon-computed; renders the same content as the Mac
// sidebar's WorkspaceRow + GitDashboardContent) ---

// gitBadges is the collapsed row's right side: branch, dirty count, tracking ↑↓.
function gitBadges(g) {
  if (!g || !g.isGitRepo) return "";
  const files = changedFiles(g);
  let out = `<span class="branch">${esc(g.branch)}</span>`;
  if (files.length) out += `<span class="dirty">●${files.length}</span>`;
  if (g.ahead) out += `<span class="ab">↑${g.ahead}</span>`;
  if (g.behind) out += `<span class="ab">↓${g.behind}</span>`;
  return out;
}

// gitDetail is the expanded block: repo path, tracking line, vs-default line,
// then "Clean" or the changed-file sections.
function gitDetail(ws) {
  const g = ws.git;
  let out = `<div class="git-detail">`;
  out += `<div class="gd-path">${esc(abbrevPath(ws.repoPath))}</div>`;
  if (!g.isGitRepo) {
    return out + `<div class="gd-none">⊘ Not a git repository</div></div>`;
  }
  if (g.trackingBranch) {
    out += `<div class="gd-track">${esc(g.branch)} → ${esc(g.trackingBranch)}` +
      ab(g.ahead, g.behind) + `</div>`;
  }
  if (g.defaultBranch && g.branch !== g.defaultBranch) {
    out += `<div class="gd-vsdef">vs ${esc(g.defaultBranch)}` +
      ab(g.aheadOfDefault, g.behindDefault) + `</div>`;
  }
  const files = changedFiles(g);
  if (!files.length) {
    out += `<div class="gd-clean">✓ Clean — no changes</div>`;
  } else {
    out += fileSection("Staged", g.stagedFiles, "staged");
    out += fileSection("Modified", g.modifiedFiles, "modified");
    out += fileSection("Deleted", g.deletedFiles, "deleted");
    out += fileSection("Untracked", g.untrackedFiles, "untracked");
  }
  return out + `</div>`;
}

function fileSection(title, files, cls) {
  if (!files || !files.length) return "";
  const rows = files
    .map((f) => `<div class="gd-file" title="${esc(f.path)}">${esc(basename(f.path))}</div>`)
    .join("");
  return `<div class="gd-sect ${cls}">${title} (${files.length})</div>` + rows;
}

function ab(ahead, behind) {
  return (ahead ? ` <span class="ab">↑${ahead}</span>` : "") +
    (behind ? ` <span class="ab">↓${behind}</span>` : "");
}

function changedFiles(g) {
  return [...(g.stagedFiles || []), ...(g.modifiedFiles || []), ...(g.deletedFiles || []), ...(g.untrackedFiles || [])];
}

function basename(p) { return p.split("/").pop(); }

// abbrevPath shortens the daemon-side home prefix, like the Mac's "~" display.
function abbrevPath(p) { return (p || "").replace(/^\/(?:Users|home)\/[^/]+/, "~"); }

// Aggregate a workspace's per-pane firehose attention into one row signal:
// needs_input wins over done. Only panes the workspace still lists count, so a
// closed pane's stale state can't keep a row lit (the 5s poll self-corrects).
// An agent's done goes off a minute after it finished (flash.js); the 5s poll
// redraws, so it ends within seconds of that.
function wsAttention(ws) {
  const per = state.attn[ws.id];
  if (!per) return "";
  const claims = state.claims[ws.id] || {};
  const F = window.ccmuxFlash;
  const shows = (p) => !F || F.doneShows(isAgentWs(ws), (claims[p.id] || {}).since || 0, Date.now());
  let done = false;
  for (const p of ws.panes || []) {
    if (per[p.id] === "needs_input") return "needs_input";
    if (per[p.id] === "done" && shows(p)) done = true;
  }
  return done ? "done" : "";
}

// --- terminal ---
function ensureTerm() {
  if (state.term) return;
  state.term = new Terminal({
    fontSize: 12, // the Mac terminal's size (RemoteTermController), and the board's tiles
    fontFamily: 'Menlo, Monaco, "SF Mono", monospace',
    cursorBlink: true,
    scrollback: 5000,
    allowProposedApi: true,
    // An app that captures the mouse (Claude Code's wheel-only mode, opencode)
    // gets the wheel as wheel reports, which xterm.js does on its own. Option-
    // drag still selects text on a Mac, as in the Mac lens (reportsMouse in
    // MacTerminalView); elsewhere xterm.js's Shift does the same.
    macOptionClickForcesSelection: true,
    theme: { background: "#1c1e22", foreground: "#d9dbdd", cursor: "#d9dbdd" },
  });
  state.fit = new FitAddon.FitAddon();
  state.term.loadAddon(state.fit);
  state.term.open($("terminal"));
  // A finger scrolls Claude and opencode too, which xterm.js leaves to the
  // mouse once they capture it (touchscroll.js).
  if (window.ccmuxTouchScroll) window.ccmuxTouchScroll.attach(state.term);
  state.term.onData((d) => sendInput(d));
  // Shift+Enter starts a new line in Claude instead of sending (keys.js).
  if (window.ccmuxKeys) window.ccmuxKeys.attach(state.term, sendInput);
  window.addEventListener("resize", scheduleFit);
  // visibility/focus does double duty:
  //  - push suppression: while actively watching, the daemon holds notifications;
  //    hidden/blurred clears focus so a "needs input" push comes through.
  //  - sizing: when THIS device becomes the active view, re-fit and re-drive its
  //    size so the pane matches this screen. Switching back to a desktop after
  //    driving from a phone must never leave the pane crushed.
  document.addEventListener("visibilitychange", onActivity);
  window.addEventListener("focus", onActivity);
  window.addEventListener("blur", onActivity);
}

// onActivity fires on visibility/focus/blur: always report focus (suppression),
// and re-fit whenever this view is visible. Being visible (foreground) is enough
// reason to re-assert our size — gating on document.hasFocus() was unreliable on
// iOS PWAs, so returning to the phone never re-drove and the pane stayed at the
// width a desktop had set (distorted). Re-fitting on visible auto-corrects it.
function onActivity() {
  reportFocus();
  if (document.visibilityState === "visible") {
    scheduleFit();
    // Returning to the view should land on the latest output, not mid-scroll.
    // Run after the debounced fit reflows (fit can shift the scroll position).
    setTimeout(scrollBottom, 140);
  }
}

// Focus and presence are two different claims. Focus says which pane this lens is
// watching, and clears that workspace's flash. Presence says this screen could
// show a notification at all — for a browser, that it is visible rather than
// buried or backgrounded. A visible-but-unfocused tab still counts as present:
// the person is at the machine, which is what the daemon needs to know before it
// decides whether to alert here or buzz their phone.
//
// The attention board never names a pane: a tile on show is not a look at its
// workspace, or every claim would retire the moment it reached the board. It
// still reports presence. Tiles leave by "acted" instead (board.js).
function reportFocus() {
  const visible = document.visibilityState === "visible";
  const watching = visible && document.hasFocus() && !state.board;
  send({ t: "focus", pane: watching ? state.paneId : "", present: visible });
}

// Below this the pane is considered broken, not a real request — a transient
// zero-size layout pass or a pathologically narrow container. We skip driving the
// shared pane to it (leave it at its current width) rather than crush everyone.
const MIN_COLS = 20;
let fitTimer = null;

// scheduleFit debounces fit() so a burst of resize/visibility events collapses
// into one resize frame.
function scheduleFit() {
  clearTimeout(fitTimer);
  fitTimer = setTimeout(doFit, 80);
}

// fitLocal sizes the terminal to its box now, telling the daemon nothing.
function fitLocal() {
  if (state.fit) try { state.fit.fit(); } catch (_) {}
}

function doFit() {
  if (!state.fit || !state.term) return;
  const before = state.term.cols + "x" + state.term.rows;
  fitLocal();
  const { cols, rows } = state.term;
  if (!cols || !rows || cols < MIN_COLS) return;
  send({ t: "resize", pane: state.paneId, cols, rows });
  // Our own screen changed size. What is on it was drawn at the old size, and
  // when the pane already had the new one the daemon sees no change and sends
  // no fresh screen, so ask for one (it coalesces with a resize's own).
  if (cols + "x" + rows !== before) send({ t: "repaint", pane: state.paneId });
  // We just drove the pane to our width — reflect it immediately (the daemon's
  // pane-size broadcast will confirm) so the "take over" affordance clears.
  state.paneCols = cols;
  updateTakeover();
}

// --- "take over": reclaim the shared pane's size when another lens drove it to a
// width this view can't show 1:1. Available on every platform (a phone drives it
// narrow, a desktop drives it wide) — shown whenever the daemon's authoritative
// pane width differs from what this view fits. ---
function updateTakeover() {
  const stale = !!state.paneId && !!state.term &&
    state.term.cols > 0 && state.paneCols > 0 && state.paneCols !== state.term.cols;
  document.getElementById("app").classList.toggle("stale", stale);
}

function scrollBottom() { try { state.term && state.term.scrollToBottom(); } catch (_) {} }

// Reclaim: re-attach the current pane, which resets + re-seeds and re-fits to this
// screen's width (the same thing selecting the session in the drawer does).
function takeOver() {
  document.getElementById("app").classList.remove("stale");
  if (state.wsId) attach(state.wsId, state.paneId);
}

// --- mobile session drawer (off-canvas sidebar) ---
function closeDrawer() { document.getElementById("app").classList.remove("drawer-open"); }
function toggleDrawer() { document.getElementById("app").classList.toggle("drawer-open"); }
// Desktop is unaffected (drawer-open only acts inside the mobile media query).
function openDrawer() { document.getElementById("app").classList.add("drawer-open"); }

// paneColsOf returns a pane's daemon-reported width (0 if unknown).
function paneColsOf(paneId) {
  const p = state.panes.find((x) => x.id === paneId);
  return (p && p.cols) || 0;
}

// --- attach / websocket ---
// How long the sidebar feed waits before it dials again: longer after each
// failure in a row, from 2s up to 30s (retry.js). The terminal socket does
// not back off; see attach's onclose.
const feedRetry = window.ccmuxRetry.backoff();

// isAsleep: the daemon lists the session as sleeping (cold), so it has no
// panes to attach to and refuses an attach with 409.
const isAsleep = (id) => (wsById(id) || {}).status === "cold";

// showAsleep is the view of a session that fell asleep while it was shown:
// put to sleep from another lens, or cooled by a blip the daemon heals within
// 30s. It is not dialed (the daemon refuses an attach with 409, and a refused
// retry every 2s made Chrome hold back every new connection from the windows
// sharing this process) and not woken behind anyone's back. The terminal says
// so; resumeIfAwake attaches again once the daemon lists it awake, and a click
// on its row wakes it now.
function showAsleep(wsId) {
  state.asleepWait = true;
  state.panes = [];
  state.paneId = null;
  if (window.ccmuxAgentChat) window.ccmuxAgentChat.hide();
  renderTabs();
  $("harness-bar").classList.add("hidden");
  $("empty").style.display = "none";
  ensureTerm();
  state.term.reset();
  const name = (wsById(wsId) || {}).name || wsId;
  state.term.write(`\r\n  ${name} is asleep. Click its row in the list to wake it.\r\n`);
  console.info("ccmux: not attaching, the session is asleep:", wsId);
  renderList();
}

// resumeIfAwake attaches the session showAsleep is waiting on once the daemon
// lists it awake again. Runs after every list read.
function resumeIfAwake() {
  if (state.asleepWait && state.wsId && wsById(state.wsId) && !isAsleep(state.wsId)) attach(state.wsId, state.wantPane);
}

// leaveForAttach puts down what the page holds before attaching wsId.
function leaveForAttach(wsId) {
  // Another session: its notes are not these. A re-attach of this one (a
  // reconnect, take over) leaves them on screen.
  if (wsId !== state.wsId && window.ccmuxScratch) window.ccmuxScratch.hide();
  state.paneCols = 0;
  if (state.conn) { state.conn.close(); state.conn = null; }
}

async function attach(wsId, wantPane) {
  closeDrawer(); // selecting a session on mobile dismisses the drawer
  leaveForAttach(wsId);
  state.wsId = wsId;
  state.wantPane = wantPane;
  if (window.ccmuxPage) window.ccmuxPage.onAttach(wsId);
  // The reconnect below comes back through here, so a session that fell
  // asleep ends its retry loop too.
  if (isAsleep(wsId)) return showAsleep(wsId);
  state.asleepWait = false;
  if (state.iconsFor !== wsId) loadHarnessIcons(wsId);
  if (!state.board) delete state.attn[wsId]; // opening it marks its attention seen; a board tile does not
  $("empty").style.display = "none";
  $("harness-bar").classList.add("hidden"); // re-derived from the next hello
  ensureTerm();
  state.term.reset();
  // Before the socket opens, not on its debounced fit: the first screen lands
  // right after hello, and a fresh page's terminal is still xterm's 80x24 then.
  // That screen stayed squeezed into 80 columns after every release, which
  // reloads open pages (update.js), until the session was opened again.
  fitLocal();
  renderList();

  // If the workspace names a host we don't know yet (joined after boot), refresh
  // the registry so we can dial it directly rather than mis-routing to the hub.
  const ws = state.workspaces.find((w) => w.id === wsId);
  if (ws && ws.host && !state.hosts[ws.host]) await fetchHosts();

  const q = `workspace=${wsId}&user=${encodeURIComponent(getUser())}&device=web`;
  const conn = new WebSocket(`${attachOrigin(ws)}/v1/attach?${q}`);
  conn.onmessage = onMessage;
  conn.onopen = () => scheduleFit();
  // The daemon now reaps a lens that stops answering pings, which a suspended tab
  // does: backgrounded on a phone, bfcached, or on a laptop that slept. Before
  // that deadline existed this socket could sit dead-but-open indefinitely and
  // nobody noticed; now it is closed deterministically, so it MUST come back.
  //
  // Without this the failure is the nastiest shape available: the firehose has
  // always reconnected on its own, so the sidebar keeps flashing and the app
  // looks alive while the terminal is a dead rectangle eating every keystroke,
  // recoverable only by a page reload the user has no reason to try.
  conn.onclose = () => {
    if (state.conn !== conn) return; // superseded by a newer attach
    state.conn = null;
    // A flat 2s, not retry.js's backoff: while this socket is down every
    // keystroke is dropped with nothing on screen to say so, so it must come
    // back fast. A sleeping session never gets here (showAsleep).
    setTimeout(() => { if (!state.conn && state.wsId === wsId) attach(wsId, state.paneId); }, 2000);
  };
  conn.onerror = (e) => console.debug("attach socket failed", wsId, e);
  state.conn = conn;
}

function onMessage(ev) {
  const m = JSON.parse(ev.data);
  switch (m.t) {
    case "hello":
      state.panes = m.panes || [];
      // A pane asked for that is gone (just closed) falls to the first one.
      state.paneId = (state.panes.some((p) => p.id === state.wantPane) && state.wantPane) ||
        (state.panes[0] && state.panes[0].id) || null;
      state.wantPane = null;
      state.paneCols = paneColsOf(state.paneId);
      // After paneId is settled, not before: a notice belongs to its pane and
      // keeps its own expiry, so switching away and back inside the window
      // still shows it — the same rule as the Mac lens's per-pane overlay.
      renderPaneNotice();
      renderTabs();
      updateHarnessBar();
      scheduleFit();
      reportFocus();
      updateTakeover();
      break;
    case "snapshot":
    case "output":
      if (m.pane === state.paneId && m.data) {
        const bytes = b64ToBytes(m.data);
        // A snapshot is a fresh screen (attach or lag-reseed): jump to the bottom so
        // we land on the latest output, not mid-scrollback. Plain output preserves
        // the user's scroll position (xterm only auto-follows when already at bottom).
        if (m.t === "snapshot") state.term.write(bytes, scrollBottom);
        else state.term.write(bytes);
      }
      break;
    case "pane-size":
      if (m.pane === state.paneId) { state.paneCols = m.cols || 0; updateTakeover(); }
      break;
    case "attention":
      setAttention(m.pane, m.state);
      break;
    case "presence":
      renderPresence(m.clients || []);
      break;
    case "pane-added":
    case "pane-closed":
      attach(state.wsId, state.paneId); // simplest correct refresh
      break;
    case "notice":
      // Something the user should know about the action they just took —
      // today only a paste that was cut short. Transient by design: it
      // describes a past event, so it clears itself rather than becoming
      // furniture. Mirrored in the Mac lens (HostedTerminalPaneView).
      showPaneNotice(m.pane, m.notice);
      break;
    case "clipboard":
      // tmux copy-mode copied in this workspace (selection = copy): mirror it
      // to the OS clipboard. Best-effort — a browser may refuse the write
      // without a recent user gesture, and a refusal must stay silent.
      if (m.data && navigator.clipboard) {
        // console.debug, not user-visible: refusals stay silent for users but
        // diagnosable in devtools (gesture policy, focus, permissions).
        navigator.clipboard.writeText(new TextDecoder().decode(b64ToBytes(m.data)))
          .catch((e) => console.debug("clipboard write refused:", e));
      }
      break;
  }
}

function send(obj) {
  if (state.conn && state.conn.readyState === 1 && state.paneId) {
    state.conn.send(JSON.stringify(obj));
  }
}

function sendInput(data) {
  send({ t: "input", pane: state.paneId, data: bytesToB64(new TextEncoder().encode(data)) });
}

// typeIntoPane types text into the pane {ws, pane} named when it was asked
// for, not whichever pane is open now, and says false when it cannot: the
// socket is down, or this window has moved to another session since.
// paste.js uses it after an upload that may take seconds.
function typeIntoPane(to, text) {
  if (!to || to.ws !== state.wsId || !state.conn || state.conn.readyState !== 1) return false;
  state.conn.send(JSON.stringify({ t: "input", pane: to.pane, data: bytesToB64(new TextEncoder().encode(text)) }));
  return true;
}

// The daemon serves panes in the shared tab order. Another lens dragging a tab
// reaches us as a workspace-status refresh, so re-sort the open session's tabs
// to the served order here; the pane objects themselves are kept (their
// attention and titles ride on them). Returns true when anything moved.
function syncPaneOrder(ws) {
  const rank = new Map(ws.panes.map((p, i) => [p.id, i]));
  const sorted = state.panes.slice().sort((a, b) => (rank.get(a.id) ?? 1e9) - (rank.get(b.id) ?? 1e9));
  if (sorted.every((p, i) => p === state.panes[i])) return false;
  state.panes = sorted;
  return true;
}

// --- tab reorder: drag a tab along the strip ---
// Mouse and pen start after 6px of sideways movement; touch holds 350ms first
// so a tap or a scroll is never read as a drag. The new order goes to the
// daemon, which renumbers and tells every other lens (the Mac's tab strips
// re-sort from the same served order).
// Listeners live on the window, not the button: the strip is rebuilt by
// renderTabs on every title or order change, and a listener on a button
// that gets replaced mid-drag never sees the release. renderTabs itself holds
// off while a drag is live and runs once it ends.
function wireTabDrag(btn, paneId) {
  btn.onpointerdown = (e) => {
    if (e.button !== 0) return;
    const start = { x: e.clientX, y: e.clientY };
    const touch = e.pointerType === "touch";
    let live = false, slot = null, hold = null;
    // Capture keeps the button as the target through the release, so the click
    // that follows can be told apart from a plain click. Best effort: a pointer
    // the browser will not capture still reports through the window listeners.
    try { btn.setPointerCapture(e.pointerId); } catch (_) { /* window listeners suffice */ }
    const arm = () => {
      live = true;
      state.tabDrag = paneId; // the context menu and pane switch stay shut while a drag is live
      btn.classList.add("dragging");
    };
    const move = (ev) => {
      if (!live) {
        if (touch) { if (Math.hypot(ev.clientX - start.x, ev.clientY - start.y) > 8) stop(); return; }
        if (Math.abs(ev.clientX - start.x) < 6) return;
        arm();
      }
      slot = markDropSlot(ev.clientX);
    };
    const up = () => { const done = live && slot !== null; stop(); if (done) movePaneTab(paneId, slot); };
    const cancel = () => stop(); // the browser took the gesture: never a drop
    const stop = () => {
      clearTimeout(hold);
      // The click that follows a release must still see the drag; then redraw
      // whatever renderTabs skipped while it was live.
      if (live) setTimeout(() => { state.tabDrag = null; renderTabs(); }, 0);
      live = false;
      btn.classList.remove("dragging");
      clearDropSlot();
      window.removeEventListener("pointermove", move);
      window.removeEventListener("pointerup", up);
      window.removeEventListener("pointercancel", cancel);
    };
    if (touch) hold = setTimeout(arm, 350);
    window.addEventListener("pointermove", move);
    window.addEventListener("pointerup", up);
    window.addEventListener("pointercancel", cancel);
  };
}

// markDropSlot maps an x position to an index in the strip (the number of tabs
// whose middle lies left of it, the dragged one counted too, which keeps the
// maths the same as the Mac's: the move removes it first, then inserts) and
// draws the marker there. Returns the index.
function markDropSlot(x) {
  const tabs = clearDropSlot();
  const slot = tabs.filter((t) => { const r = t.getBoundingClientRect(); return r.left + r.width / 2 < x; }).length;
  if (slot < tabs.length) tabs[slot].classList.add("drop-before");
  else if (tabs.length) tabs[tabs.length - 1].classList.add("drop-after");
  return slot;
}

function clearDropSlot() {
  const tabs = [...document.querySelectorAll("#tabs .tab")];
  tabs.forEach((t) => t.classList.remove("drop-before", "drop-after"));
  return tabs;
}

// movePaneTab applies the drop locally first (the strip must not wait on the
// round trip), then sends the whole order. A refusal or a dead connection is
// said out loud like every other write here, then the strip re-syncs from the
// daemon so it never shows an order the daemon does not hold.
async function movePaneTab(paneId, slot) {
  const ids = state.panes.map((p) => p.id);
  const from = ids.indexOf(paneId);
  if (from < 0) return;
  ids.splice(from, 1);
  const at = slot > from ? slot - 1 : slot;
  if (at === from) return;
  ids.splice(at, 0, paneId);
  const byId = new Map(state.panes.map((p) => [p.id, p]));
  state.panes = ids.map((id) => byId.get(id));
  renderTabs();
  try {
    const r = await fetch(`/v1/workspaces/${state.wsId}/pane-order`, {
      method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ order: ids }),
    });
    if (!r.ok) throw new Error(await r.text());
  } catch (err) {
    alert("reorder tabs: " + (err.message || err));
    fetchWorkspaces();
  }
}

// --- pane tabs ---

// updateTip explains a pane's claudeUpdate mark (manager/autorestart.go): the
// daemon restarts a Claude that is behind the installed version once it has
// sat idle an hour, and says so when it cannot do that one.
function updateTip(mark) {
  if (mark === "pending") return "Claude update installed: restarts by itself after an hour idle";
  if (mark === "manual") return "Claude update installed: ccmux can't restart this one by itself, restart it by hand";
  return "";
}

function renderTabs() {
  if (state.tabDrag) return; // rebuilding the strip mid-drag would orphan the drag
  const tabs = $("tabs");
  tabs.innerHTML = "";
  state.panes.forEach((p, i) => {
    const b = document.createElement("button");
    // A dormant pane hosted a Claude session that has exited. The pane is alive,
    // which is exactly why it needs saying — nothing else tells it apart from a
    // working session.
    const scratch = window.ccmuxScratch && window.ccmuxScratch.isOpen();
    b.className = "tab" + (p.id === state.paneId && !scratch ? " active" : "") +
      " att-" + (p.attention || "idle") + (p.dormant ? " dormant" : "") +
      (p.claudeUpdate ? " update-" + p.claudeUpdate : "");
    b.dataset.pane = p.id;
    b.title = [p.agent ? `agent ${p.agent}` + (p.atShell || p.dormant ? " — asleep" : "")
      : p.dormant ? "Claude exited — shell only" : "", updateTip(p.claudeUpdate)].filter(Boolean).join("\n");
    const icon = document.createElement("span");
    icon.className = "tab-icon";
    icon.textContent = tabIcon(p);
    const label = document.createElement("span");
    label.className = "tab-label";
    label.textContent = (p.agent ? "⚙ " : "") + (p.title || `pane ${i + 1}`) + (p.claudeUpdate ? " ↻" : "");
    b.append(icon, label);
    if (closableTab(p)) b.appendChild(tabClose(p));
    b.setAttribute("aria-label", label.textContent); // a phone shows the icon only
    // A drop ends with a click on the same button; that click must not switch panes.
    b.onclick = () => { if (!state.tabDrag) showPane(p.id); };
    b.oncontextmenu = (e) => { e.preventDefault(); if (!state.tabDrag) openPaneLLMMenu(p.id, e.clientX, e.clientY); };
    wireTabDrag(b, p.id);
    tabs.appendChild(b);
  });
  renderScratchTab();
}

// showPane puts pane id of the open session on screen, leaving the
// scratchpad if that is what was showing.
function showPane(paneId) {
  if (window.ccmuxScratch) window.ccmuxScratch.hide();
  attach(state.wsId, paneId);
}

// renderScratchTab draws the Scratchpad tab after the pane tabs (scratchpad.js). It
// sits outside #tabs so the drag code, which counts #tabs .tab, never sees it.
// An agent's session has none: the notes are for people working in a repo.
function renderScratchTab() {
  const t = $("scratch-tab");
  if (!t || !window.ccmuxScratch) return;
  const ws = state.workspaces.find((w) => w.id === state.wsId);
  t.classList.toggle("hidden", !state.wsId || !!(ws && isAgentWs(ws)));
  t.classList.toggle("active", window.ccmuxScratch.isOpen());
}

// closableTab says whether a tab gets the hover ×. Not the only tab: closing
// the last pane archives the whole session (manager.KillPane), which is
// "Close Session"'s job. Not an agent's: an agent is put to sleep from its
// window, and killing its pane under it would skip that.
function closableTab(p) {
  return state.panes.length > 1 && !p.agent;
}

// tabClose is the × a desktop shows on a hovered tab (style.css hides it on
// a phone and on a touch screen, which cannot hover).
function tabClose(p) {
  const x = document.createElement("span");
  x.className = "tab-close";
  x.textContent = "×";
  x.title = "Close tab";
  x.setAttribute("role", "button");
  x.onpointerdown = (e) => e.stopPropagation(); // a press on the × is not a tab drag
  x.onclick = (e) => { e.stopPropagation(); closePaneTab(p); };
  return x;
}

// closePaneTab ends a pane and whatever runs in it. A plain terminal goes at
// once; a harness or a dev server asks first, since a stray click would stop
// it. Then it re-attaches for the new tab list, as spawnWebPane does: no
// daemon sends a pane-closed frame, whatever the onMessage case suggests.
async function closePaneTab(p) {
  const name = p.title || p.harness || "this tab";
  if ((p.harness || p.devServer) && !confirm(`Close "${name}"? What runs in it stops.`)) return;
  const wsId = state.wsId;
  try {
    const r = await fetch(`/v1/workspaces/${wsId}/panes/${p.id}`, { method: "DELETE" });
    if (!r.ok) { alert("close tab: " + (await r.text())); return; }
  } catch (e) {
    alert("close tab: " + e.message); // the daemon is away: say so, the tab stays
    return;
  }
  if (state.wsId !== wsId) return; // moved on while it closed
  const show = p.id === state.paneId ? neighbourPane(p.id) : state.paneId;
  attach(wsId, show);
}

// neighbourPane is the tab shown after closing paneId: the one to its right,
// else the one to its left.
function neighbourPane(paneId) {
  const i = state.panes.findIndex((x) => x.id === paneId);
  const next = state.panes[i + 1] || state.panes[i - 1];
  return next ? next.id : null;
}

// tabIcon is the glyph a tab shows for what runs in it: the harness's icon
// from Settings (✳ claude, ⌬ opencode, …), its first letter when it has none,
// ▶ for the dev server, and >_ for a plain terminal. A phone shows only this.
function tabIcon(p) {
  if (p.devServer) return "▶";
  if (!p.harness) return ">_";
  return state.harnessIcons[p.harness] || p.harness.charAt(0).toUpperCase();
}

// rememberHarnessIcons keeps the harness icons from a settings read for the
// tab strip, and redraws it when they changed.
function rememberHarnessIcons(cfg) {
  const icons = {};
  for (const h of (cfg && cfg.harnesses) || []) if (h.icon) icons[h.name] = h.icon;
  if (JSON.stringify(icons) === JSON.stringify(state.harnessIcons)) return;
  state.harnessIcons = icons;
  renderTabs();
}

// loadHarnessIcons reads a session's harness icons once per session shown.
// A failed read keeps the icons there were; tabs then show letters at worst.
async function loadHarnessIcons(wsId) {
  state.iconsFor = wsId;
  const ws = state.workspaces.find((w) => w.id === wsId);
  try {
    rememberHarnessIcons(await (await fetch(settingsURLFor(ws))).json());
  } catch (e) {
    console.warn("harness icons unreadable; tabs show letters:", e);
    state.iconsFor = null; // try again on the next attach
  }
}

// Right-click on a pane tab: pick which LLM account answers THIS pane. The
// choice applies to the pane's next request — no restart. "Use the harness
// order" clears the override so the pane falls back to what its harness
// declares (and, with no harness, to the default account).
//
// The header shows the order the proxy would ACTUALLY try, which is not the
// configured order: an account at its limit has already sunk to the back of
// it, so a pane answering from the second account says so here.
// Both lenses say the same thing about a pane whose routing will not resolve;
// they just learn it differently. This lens re-fetches the pane's own route
// endpoint on every menu open, so a 503 there IS the failure and carries the
// reason. The Mac lens has no per-menu fetch, which is why the daemon also
// ships llmPaneOrderErrors in the settings blob for it to read. Same rule,
// same words on screen, different transport — not two rules.
async function openPaneLLMMenu(paneId, x, y) {
  let cur;
  let unresolved = "";
  try {
    const r = await fetch(`/v1/panes/${paneId}/llm-route`);
    if (r.status === 404) return; // pane unknown — no menu to offer
    if (!r.ok) {
      // The daemon could not resolve this pane's routing, so its next request
      // will fail. Say so: silently offering no menu made a broken pane look
      // like one the proxy is not mounted for.
      //
      // Unwrapped: writeError sends {"error": msg}, so the raw body would put
      // JSON braces and escaped quotes in a context-menu line — and the Mac
      // lens prints the bare reason for this same state.
      const body = (await r.text()).trim();
      let reason = "";
      try { reason = (JSON.parse(body) || {}).error || ""; } catch (_) { reason = body; }
      unresolved = reason || body || `HTTP ${r.status}`;
    } else {
      cur = await r.json();
    }
  } catch (e) { unresolved = String(e.message || e); }
  if (unresolved) {
    // Still offer every account, as the Mac lens does for this state: pinning
    // a working one is how the pane gets repaired, and a menu with no actions
    // leaves the user nowhere to go. The daemon refuses an incompatible pick
    // with its own message.
    let names = [];
    try {
      names = ((await (await fetch("/v1/settings")).json()).llmAccounts || []).map((a) => a.name);
    } catch (_) { /* the header still names the failure */ }
    cur = { route: "unresolved", accounts: names, order: [] };
  }

  const menu = $("ctx-menu");
  menu.innerHTML = "";
  const line = document.createElement("div");
  line.className = "host-line";
  line.textContent = unresolved
    ? "LLM route · unresolved: " + unresolved
    : "LLM route · now: " + cur.effective;
  menu.appendChild(line);
  const order = cur.order || [];
  if (order.length > 1) {
    const chain = document.createElement("div");
    chain.className = "host-line";
    chain.textContent = "then: " + order.slice(1).join(" → ");
    chain.title = "Tried in this order when one hits its limit";
    menu.appendChild(chain);
  }

  const put = async (route) => {
    const r = await fetch(`/v1/panes/${paneId}/llm-route`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ route }),
    });
    if (!r.ok) alert("llm route: " + (await r.text()));
  };
  const add = (label, route, marked) => {
    const b = document.createElement("button");
    b.textContent = (marked ? "● " : "○ ") + label;
    b.onclick = () => { closeWsMenu(); put(route); };
    menu.appendChild(b);
  };
  add("Use the harness order", "", cur.route === "");
  for (const name of cur.accounts || []) add(name, name, cur.route === name);

  showMenuAt(menu, x, y);
}

// settingsURLFor targets the settings of the host that owns ws (the hub
// proxies member hosts), with the repoPath so the response carries the
// folder rule's preselection.
function settingsURLFor(ws) {
  const base = ws && ws.host ? `/v1/hosts/${encodeURIComponent(ws.host)}/settings` : "/v1/settings";
  return ws && ws.repoPath ? `${base}?repoPath=${encodeURIComponent(ws.repoPath)}` : base;
}

// hasChat: the harnesses whose agent instances serve a chat (daemon:
// agent.HasChat). Keep the three copies identical.
function hasChat(harness) { return harness === "opencode" || harness === "claude"; }

// --- harness bar: shown when the ACTIVE pane sits at a bare shell with no
// harness recorded — the empty-new-workspace flow, and any shell tab. The
// folder rule only PRESELECTS (highlighted); nothing ever auto-starts. ---
async function updateHarnessBar() {
  const bar = $("harness-bar");
  const paneId = state.paneId;
  const p = state.panes.find((x) => x.id === paneId);
  // A pane at a bare shell gets the bar: no harness yet = "Start here:",
  // harness recorded but exited (its shell is back) = "Restart:".
  if (p && p.agent) {
    setHarnessBar(false);
    // The chat rides the instance's server: opencode's own, or the claude
    // sidecar's. Same rule as agent.HasChat in the daemon and the Mac's
    // PaneContentView; a harness without one keeps its terminal.
    if (hasChat(p.harness)) window.ccmuxAgentChat.show(p, state.workspaces.find((w) => w.id === state.wsId));
    else window.ccmuxAgentChat.hide();
    return;
  }
  window.ccmuxAgentChat.hide();
  if (!p || p.devServer || !(p.atShell || p.dormant)) { setHarnessBar(false); return; }
  const ws = state.workspaces.find((w) => w.id === state.wsId);
  let cfg;
  try {
    cfg = await (await fetch(settingsURLFor(ws))).json();
  } catch (_) { setHarnessBar(false); return; }
  rememberHarnessIcons(cfg);
  if (state.paneId !== paneId) return; // switched tabs while fetching
  const harnesses = cfg.harnesses || [];
  if (!harnesses.length) { setHarnessBar(false); return; }
  const suggested = p.harness || cfg.resolvedHarness;
  bar.innerHTML = "";
  const label = document.createElement("span");
  label.className = "harness-label";
  label.textContent = p.harness ? "Restart:" : "Start here:";
  bar.appendChild(label);
  for (const h of harnesses) {
    const b = document.createElement("button");
    b.className = "harness-btn" + (h.name === suggested ? " suggested" : "");
    b.textContent = (h.icon ? h.icon + " " : "") + h.name;
    b.title = h.command;
    b.onclick = () => startHarness(paneId, h.name);
    bar.appendChild(b);
  }
  const x = document.createElement("button");
  x.className = "harness-dismiss";
  x.title = "Keep the shell";
  x.innerHTML = "&times;";
  x.onclick = () => setHarnessBar(false);
  bar.appendChild(x);
  setHarnessBar(true);
}

function setHarnessBar(show) {
  $("harness-bar").classList.toggle("hidden", !show);
  scheduleFit(); // the bar changes the terminal's height
}

async function startHarness(paneId, name) {
  setHarnessBar(false);
  const r = await fetch(`/v1/panes/${paneId}/harness`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ harness: name }),
  });
  if (!r.ok) {
    alert(`start ${name}: ` + (await r.text()));
    updateHarnessBar(); // the pane may still deserve the offer
  }
}

// --- Agents: the window's catalog (icons, drift) and the wake call the
// sidebar menu uses. The pane-level chat view lives in agentchat.js. ---
const agentCatalog = { winId: "", list: [], at: 0 };
// The Settings window's Agents tab (settingsagents.js) says when it read or
// changed the bases; what is held here is then out of date.
document.addEventListener("ccmux-agents-changed", () => { agentCatalog.at = 0; });

// windowOf is the shared window a workspace sits in (by its group name), or
// null for an ungrouped one — agents live on windows, so an ungrouped agent
// session has nothing to be woken through.
function windowOf(ws) {
  const g = ((ws && ws.group) || "").toLowerCase();
  return g ? state.windows.find((w) => w.name.toLowerCase() === g) || null : null;
}

async function fetchWindowAgents(winId) {
  if (agentCatalog.winId === winId && Date.now() - agentCatalog.at < 5000) return agentCatalog.list;
  try {
    const r = await fetch(`/v1/windows/${winId}/agents`);
    if (!r.ok) {
      const body = await r.text();
      let msg = body;
      try { msg = JSON.parse(body).error || body; } catch (_) { /* not JSON */ }
      throw new Error(msg);
    }
    const list = (await r.json()).agents || [];
    Object.assign(agentCatalog, { winId, list, at: Date.now(), error: "" });
    return list;
  } catch (e) {
    // The daemon fails the whole list on one broken base and names it; keep
    // that visible instead of an empty menu that looks like "no agents".
    console.warn("agents:", e.message);
    Object.assign(agentCatalog, { winId, list: [], at: Date.now(), error: e.message });
    return [];
  }
}

// wakeAndOpen wakes an agent and shows its pane. The list is read again
// first: it still says the session is asleep, and attach refuses a sleeping one.
async function wakeAndOpen(winId, name, prompt) {
  const p = await wakeAgent(winId, name, prompt);
  if (!p || !p.id) return;
  if (p.runtime === "runner" && p.sessionId) {
    // It runs as a session: its conversation is on the thread page.
    location.href = window.ccmuxRoute.threadHref({ kind: "session", id: p.sessionId });
    return;
  }
  await fetchWorkspaces();
  openWorkspace(p.workspaceId, p.id);
}

async function wakeAgent(winId, name, prompt) {
  try {
    const r = await fetch(`/v1/windows/${winId}/agents/${encodeURIComponent(name)}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prompt, createdBy: "web" }),
    });
    if (!r.ok) throw new Error(await r.text());
    agentCatalog.at = 0; // state changed
    return await r.json();
  } catch (e) {
    alert(`agent ${name}: ` + e.message);
    updateHarnessBar(); // the pane may still deserve the composer
    return null;
  }
}

// --- "+" in the tab strip: a new pane running a harness, or a plain shell. ---
function wirePaneAdd() {
  const btn = $("pane-add");
  if (!btn) return;
  btn.onclick = async (e) => {
    e.stopPropagation(); // the document-level closer would eat the menu
    if (!state.wsId) return;
    const ws = state.workspaces.find((w) => w.id === state.wsId);
    let cfg = {};
    try { cfg = await (await fetch(settingsURLFor(ws))).json(); } catch (_) {}
    const menu = $("ctx-menu");
    menu.innerHTML = "";
    const add = (label, fn) => {
      const b = document.createElement("button");
      b.textContent = label;
      b.onclick = () => { closeWsMenu(); fn(); };
      menu.appendChild(b);
    };
    for (const h of cfg.harnesses || []) {
      add((h.icon ? h.icon + " " : "") + h.name, () => spawnWebPane({ harness: h.name }));
    }
    add("▸ Terminal", () => spawnWebPane({}));
    menu.classList.remove("hidden");
    const br = btn.getBoundingClientRect(), mr = menu.getBoundingClientRect();
    menu.style.left = Math.max(4, Math.min(br.left, window.innerWidth - mr.width - 8)) + "px";
    menu.style.top = br.bottom + 4 + "px";
  };
}
wirePaneAdd();

async function spawnWebPane(body) {
  const r = await fetch(`/v1/workspaces/${state.wsId}/panes`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ ...body, createdBy: getUser() }),
  });
  if (!r.ok) { alert("new pane: " + (await r.text())); return; }
  const p = await r.json();
  showPane(p.id); // land on the pane just asked for, out of the scratchpad too
}

function renderPresence(clients) {
  const el = $("presence");
  el.innerHTML = "";
  for (const c of clients) {
    const chip = document.createElement("span");
    chip.className = "chip" + (c.driving ? " driving" : "") + (c.readonly ? " ro" : "");
    chip.title = (c.driving ? "driving" : c.readonly ? "observing" : "attached") + (c.device ? " · " + c.device : "");
    chip.innerHTML = `<span class="ring"></span>${esc(c.user)}`;
    el.appendChild(chip);
  }
}

function setAttention(paneId, stateStr) {
  const p = state.panes.find((x) => x.id === paneId);
  if (p) p.attention = stateStr;
  const btn = document.querySelector(`.tab[data-pane="${paneId}"]`);
  if (btn) btn.className = btn.className.replace(/att-\w+/, "att-" + stateStr);
}

// --- global firehose (/v1/events): live sidebar attention for every workspace,
// so a row flashes even when we're not attached to it. Read-only; reconnects. ---
function connectFirehose() {
  fetchHosts(); // refresh the registry on each (re)connect — a host may have joined/left
  const proto = location.protocol === "https:" ? "wss" : "ws";
  // The user is part of the URL because the daemon stamps each frame's alert flag
  // for the lens it is writing to. It must be the same name the attach socket
  // sends, or presence and alerting describe two different people.
  const fh = new WebSocket(
    `${proto}://${location.host}/v1/events?user=${encodeURIComponent(getUser())}&device=web`
  );
  fh.onmessage = onFirehose;
  // Presence lives per connection on the daemon, so a fresh stream starts
  // as nobody-at-a-screen: the attention board says it again (board.js).
  fh.onopen = () => { feedRetry.ok(); if (window.ccmuxBoard) window.ccmuxBoard.reportPresence(); };
  fh.onclose = () => { state.firehose = null; setTimeout(connectFirehose, feedRetry.next()); };
  state.firehose = fh;
}

// onFirehose hands one firehose frame to the handler for its kind; a kind
// this lens does not know is ignored. A handler returns true when the sidebar
// and the attention board must redraw.
function onFirehose(ev) {
  let m;
  try { m = JSON.parse(ev.data); } catch (_) { return; }
  const handle = firehoseHandlers.get(m.t);
  if (!handle || !handle(m)) return;
  renderList();
  if (window.ccmuxBoard) window.ccmuxBoard.refresh();
}

// A workspace changed elsewhere: refresh now, don't wait for the poll.
const refetchWorkspaces = () => { fetchWorkspaces(); return false; };

const firehoseHandlers = new Map([
  ["hello", (m) => {
    if (window.ccmuxTasks) window.ccmuxTasks.refresh(); // changes missed while the socket was down
    state.attn = {};
    state.claims = {};
    for (const e of m.attention || []) noteAttention(e.workspace, e.pane, e.state, e.reason, e.since);
    return true;
  }],
  // A reminder re-sends a claim already showing so the daemon can push it.
  // Redrawing for it would restart every row's pulse, so only a lens that
  // missed the claim redraws.
  ["attention", (m) => {
    const was = (state.attn[m.workspace] || {})[m.pane];
    noteAttention(m.workspace, m.pane, m.state, m.reason, m.since);
    return !(m.remind && was === m.state);
  }],
  ["workspace-added", refetchWorkspaces],
  ["workspace-removed", refetchWorkspaces],
  ["workspace-status", refetchWorkspaces],
  ["workspace-git", refetchWorkspaces],
  // Someone saved a workspace's notes (scratchpad.js).
  ["workspace-scratchpad", (m) => { if (window.ccmuxScratch) window.ccmuxScratch.remote(m.workspace); return false; }],
  // The task queue changed (tasks.js reads it again and redraws its row).
  ["tasks-changed", () => { if (window.ccmuxTasks) window.ccmuxTasks.refresh(); return false; }],
]);

// noteAttention records one pane's attention: the bare state the sidebar
// flash reads, and the claim (why, since when) the attention board sorts by.
function noteAttention(wsId, paneId, stateStr, reason, since) {
  if (!wsId || !paneId) return;
  (state.attn[wsId] || (state.attn[wsId] = {}))[paneId] = stateStr;
  (state.claims[wsId] || (state.claims[wsId] = {}))[paneId] = { state: stateStr, reason: reason || "", since: since || 0 };
}

// --- new workspace: browse the daemon's projects root and pick a folder. The
// folders live on the daemon's filesystem (which may be a remote server), so
// the picker is fed from GET /v1/projects — never a locally typed path.
// Tapping a row drills into that folder (projects can nest); the row's +
// creates the workspace there. ".." walks back up. ---
// createHostId is the host the New-workspace picker targets: the explicit choice,
// else the hub's own node (self). "" only when there's no federation at all.
function createHostId() {
  if (state.createHost) return state.createHost;
  const self = Object.values(state.hosts).find((h) => h.self);
  return self ? self.id : "";
}

// Create endpoints route through the hub to the chosen host when federated
// (self runs local), and hit the bare routes in single-host mode.
function projectsBase() {
  const host = createHostId();
  return host ? `/v1/hosts/${encodeURIComponent(host)}/projects` : "/v1/projects";
}
function projectsURL(relPath) {
  return projectsBase() + "?path=" + encodeURIComponent(relPath);
}
function createWorkspaceURL() {
  const host = createHostId();
  return host ? `/v1/hosts/${encodeURIComponent(host)}/workspaces` : "/v1/workspaces";
}

// populateHostPicker shows the host <select> only when there's more than one
// member; picking a host re-browses that host's projects.
function populateHostPicker() {
  const sel = $("project-host");
  const hosts = Object.values(state.hosts);
  if (hosts.length < 2) { sel.classList.add("hidden"); return; }
  sel.classList.remove("hidden");
  sel.innerHTML = "";
  for (const h of hosts.sort((a, b) => (a.self ? -1 : b.self ? 1 : a.id.localeCompare(b.id)))) {
    const o = document.createElement("option");
    o.value = h.id;
    o.textContent = h.self ? `${h.id} (hub)` : h.id;
    if (!h.healthy) o.textContent += " — offline";
    sel.appendChild(o);
  }
  sel.value = createHostId();
  sel.onchange = () => { state.createHost = sel.value; browseProjects(""); };
}

function newWorkspace() {
  $("project-cmd").value = "";
  state.createHost = "";
  populateHostPicker();
  // Offer the existing window groups; free text makes a new one. Empty = auto
  // (the first Mac window adopts it).
  $("project-group").value = "";
  const options = $("project-group-options");
  options.innerHTML = "";
  for (const g of [...new Set(state.workspaces.map((w) => w.group).filter(Boolean))].sort()) {
    const o = document.createElement("option");
    o.value = g;
    options.appendChild(o);
  }
  browseProjects("");
}

async function browseProjects(relPath) {
  const status = $("project-status"), list = $("project-list"), crumb = $("project-path");
  state.projectPath = relPath; // where "new folder" creates
  $("project-modal").classList.remove("hidden");
  list.innerHTML = "";
  crumb.textContent = "/" + relPath;
  status.textContent = "Loading…";
  status.classList.remove("hidden");
  let resp;
  try {
    const r = await fetch(projectsURL(relPath));
    if (!r.ok) throw new Error((await r.text()).trim());
    resp = await r.json();
  } catch (e) {
    status.textContent = "Couldn't list projects: " + e.message;
    return;
  }
  const projects = resp.projects || [];
  if (relPath !== "") {
    const up = document.createElement("li");
    up.className = "up";
    up.innerHTML = '<span class="name">‹ back</span>';
    up.onclick = () => browseProjects(resp.parent || "");
    list.appendChild(up);
  }
  if (!projects.length) {
    status.textContent = "No folders in here.";
  } else {
    status.classList.add("hidden");
  }
  for (const p of projects) {
    const li = document.createElement("li");
    li.innerHTML =
      `<span class="name">${esc(p.name)}</span>` +
      (p.git ? '<span class="git">git</span>' : "") +
      `<button class="mk" title="New workspace in ${esc(p.name)}">+</button>`;
    li.onclick = () => browseProjects(relPath ? relPath + "/" + p.name : p.name);
    li.querySelector(".mk").onclick = (e) => {
      e.stopPropagation();
      closeProjectModal();
      createWorkspace(p);
    };
    list.appendChild(li);
  }
}

function closeProjectModal() { $("project-modal").classList.add("hidden"); }

// "Create" in the picker: a new folder in the currently browsed location —
// plain for a folder that will hold more folders, git-inited for a repo-to-be.
async function createProjectFolder() {
  const name = ($("project-folder").value || "").trim();
  if (!name) return;
  const rel = state.projectPath ? state.projectPath + "/" + name : name;
  try {
    const r = await fetch(projectsBase(), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path: rel, git: $("project-folder-git").checked }),
    });
    if (!r.ok) { alert("create folder: " + (await r.text())); return; }
  } catch (e) {
    // An unreachable daemon rejects the fetch, and an async click handler
    // swallows that: no alert, no status, the button just looks dead.
    alert("create folder failed: " + e.message);
    return;
  }
  $("project-folder").value = "";
  $("project-folder-git").checked = false;
  browseProjects(state.projectPath); // re-list; the new folder appears in place
}

async function createWorkspace(p) {
  // New workspaces open EMPTY (explicit "" overrides the daemon's resolve):
  // the harness bar then offers what to start, with the folder rule only
  // preselecting. The picker's command field stays as a one-off escape hatch
  // that types exactly what it says.
  const body = { name: p.name, repoPath: p.path, createdBy: "web", startupCommand: "" };
  const override = ($("project-cmd").value || "").trim();
  if (override) body.startupCommand = override;
  const group = ($("project-group").value || "").trim();
  if (group) body.group = group;
  const r = await fetch(createWorkspaceURL(), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!r.ok) { alert("create failed: " + (await r.text())); return; }
  const ws = await r.json();
  await fetchWorkspaces();
  openWorkspace(ws.id, null);
}

// The Settings window is not in this file: its frame is settingsmodal.js,
// and each tab is a file of its own (settingsaccounts.js and the rest).

// Opened from a notification tap (/?ws=<id>): attach straight to that workspace.
// Without a deep link there's nothing on screen but "Select a workspace", so
// surface the session list (the flyout drawer on mobile) instead of a dead end.
function bootDeepLink() {
  if (window.ccmuxPage) return window.ccmuxPage.boot(); // pagewin.js: this window's ccmux window
  const ws = new URLSearchParams(location.search).get("ws");
  if (ws) showWorkspace(ws, null);
  else openDrawer();
}

// --- boot ---
// Identity first: the firehose and a deep-link attach both send the presence
// name, and on the tailnet that name comes from the daemon, not a prompt.
// push.js awaits the same promise before its own deep-link attach (a
// notification tap), which otherwise fires on DOMContentLoaded, ahead of it.
const identityReady = askIdentityOnce();
// push.js deep-links and shares the presence name; paste.js types a pasted
// image's path into the pane it was pasted in.
window.ccmux = {
  attach: openWorkspace, getUser, identityReady,
  currentPane: () => (state.wsId && state.paneId ? { ws: state.wsId, pane: state.paneId } : null),
  typeIntoPane,
};
$("new-ws").onclick = newWorkspace;
$("project-close").onclick = closeProjectModal;
$("project-folder-mk").onclick = createProjectFolder;
$("project-folder").addEventListener("keydown", (e) => {
  if (e.key === "Enter") createProjectFolder();
});
$("project-modal").onclick = (e) => { if (e.target.id === "project-modal") closeProjectModal(); };
$("hostnames-close").onclick = () => $("hostnames-modal").classList.add("hidden");
$("hostnames-modal").onclick = (e) => { if (e.target.id === "hostnames-modal") $("hostnames-modal").classList.add("hidden"); };
$("hostnames-add").onclick = () => $("hostnames-rows").appendChild(hostnameRow("", ""));
$("hostnames-save").onclick = saveHostnames;
document.addEventListener("click", (e) => { if (!$("ctx-menu").contains(e.target)) closeWsMenu(); });
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeWsMenu(); });
$("menu-toggle").onclick = toggleDrawer;
$("drawer-backdrop").onclick = closeDrawer;
$("takeover").onclick = takeOver;
$("scratch-tab").onclick = () => window.ccmuxScratch && window.ccmuxScratch.show();
identityReady.then(() => {
  clearPhoneLeftovers();
  fetchHosts().then(fetchWorkspaces).then(bootDeepLink); // hosts first so deep-link attach dials direct
  connectFirehose();
});
setInterval(fetchWorkspaces, 5000); // reflect status/pane-count changes

// --- pane notices: short, self-clearing lines about what just happened in a
// pane. Kept apart from the daemon-warning strip because the two have opposite
// lifetimes — that one persists until someone acts, this one is history the
// moment it is read.
//
// Held PER PANE with its own expiry, not as one global banner, so this lens
// applies the identical rule to the Mac app's per-pane overlay: a notice
// belongs to the pane it happened in and lives ~10s, whether or not you are
// looking at that pane meanwhile. One strip is only how this lens draws it.
const NOTICE_LIFETIME_MS = 10000; // matches RemoteSessionService.noticeLifetime
const paneNotices = new Map(); // pane id -> {text, timer}

function showPaneNotice(pane, text) {
  if (!text || !pane) return;
  const existing = paneNotices.get(pane);
  if (existing) clearTimeout(existing.timer);
  paneNotices.set(pane, {
    text,
    timer: setTimeout(() => {
      paneNotices.delete(pane);
      renderPaneNotice();
    }, NOTICE_LIFETIME_MS),
  });
  renderPaneNotice();
}

// renderPaneNotice shows whatever the CURRENT pane has, or nothing.
//
// The strip is a block element inside the flex column that #terminal flexes
// into, so showing or hiding it changes the terminal's height by a row. There
// is no ResizeObserver anywhere in this lens — the only refit triggers are
// window resize, visibility changes, and explicit scheduleFit() calls — so
// without the call below the grid stays wrong until the user resizes the
// window. That is not cosmetic: doFit also drives the shared pane size to the
// daemon, so a stale row count would reach every other lens on the pane. Same
// reason setHarnessBar calls it.
function renderPaneNotice() {
  const el = $("pane-notice");
  const entry = paneNotices.get(state.paneId);
  const wasHidden = el.classList.contains("hidden");
  if (!entry) {
    el.classList.add("hidden");
  } else {
    el.textContent = entry.text;
    el.classList.remove("hidden");
  }
  if (wasHidden !== el.classList.contains("hidden")) scheduleFit();
}

// --- daemon health: a child the daemon started and never reaped is invisible
// everywhere but ps. Shown only when non-zero — a permanent "0 zombies" line is
// furniture the eye learns to skip.
//
// Every no-answer path hides the strip. That is the point: keeping the last
// reading on a failed poll meant the strip kept saying
// "restart the daemon" to someone who just had, for as long as it stayed down.
async function fetchDaemonHealth() {
  const el = $("daemon-warning");
  const hide = () => el.classList.add("hidden");
  let c;
  try {
    const r = await fetch("/v1/health");
    if (!r.ok) return hide(); // a 500 with a JSON body is not a clean bill
    const h = await r.json();
    c = h && h.children;
  } catch (e) {
    // Transport OR a body this lens cannot read. The second is worth a line:
    // if the field names ever move, this warning goes dark permanently and
    // nothing else would say so.
    console.warn("ccmux: /v1/health unreadable — defunct-child warning is off:", e);
    return hide();
  }
  // known:false means the daemon could not inspect itself. Say nothing rather
  // than imply a clean bill of health.
  if (!c || !c.known || !c.defunct) return hide();
  const n = c.defunct;
  el.textContent = `ccmuxd is holding ${n} defunct child process${n === 1 ? "" : "es"}. ` +
    `Restarting the daemon clears them.`;
  el.classList.remove("hidden");
}
fetchDaemonHealth();
setInterval(fetchDaemonHealth, 30000);
