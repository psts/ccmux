// ccmux web lens — a thin xterm.js client over the daemon's REST + WS API.
// No build step: vanilla JS against the globals Terminal (xterm.js) and
// FitAddon (addon-fit), served embedded from ccmuxd.
"use strict";

const state = {
  workspaces: [],
  hosts: {},         // federation: host label -> {id, addr, ...} from GET /v1/hosts
  createHost: "",    // host chosen in the New-workspace picker ("" = hub/self)
  wsId: null,
  paneId: null,      // currently rendered pane
  wantPane: null,    // pane to select after the next attach (for tab switches)
  panes: [],
  conn: null,
  term: null,
  fit: null,
  firehose: null,    // global /v1/events WS (sidebar attention, all workspaces)
  attn: {},          // wsId -> { paneId -> attentionState } from the firehose
  paneCols: 0,       // authoritative width of the current pane (from the daemon)
  gitOpen: {},       // wsId -> true when the row's changed-files list is expanded
};

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s).replace(/[<>&]/g, (c) => ({ "<": "&lt;", ">": "&gt;", "&": "&amp;" }[c]));

// A display name for presence; asked once and remembered. Tailscale identity
// will replace this on the tailnet.
function getUser() {
  let u = localStorage.getItem("ccmux-user");
  if (!u) {
    u = (prompt("Your name (for presence):", "") || "anon").trim() || "anon";
    localStorage.setItem("ccmux-user", u);
  }
  return u;
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
async function fetchWorkspaces() {
  try {
    const r = await fetch("/v1/workspaces");
    state.workspaces = (await r.json()) || [];
  } catch (_) {
    state.workspaces = [];
  }
  syncPaneTitles();
  renderList();
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
// host-stamped workspace dials wss://<host.addr>; an empty/unknown host (single-
// host, or the hub's own sessions) stays same-origin.
function attachOrigin(ws) {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const h = ws && ws.host && state.hosts[ws.host];
  if (h && h.addr) return `${proto}://${h.addr}`;
  return `${proto}://${location.host}`;
}

// Pane titles change while attached (the daemon re-derives them from tmux as
// programs start and stop); fold the refreshed titles into the open session's
// tabs. Pane add/close still re-attaches via the pane-added/pane-closed frames.
function syncPaneTitles() {
  const ws = state.workspaces.find((w) => w.id === state.wsId);
  if (!ws || !ws.panes) return;
  let changed = false;
  for (const p of state.panes) {
    const q = ws.panes.find((x) => x.id === p.id);
    if (q && q.title !== p.title) { p.title = q.title; changed = true; }
  }
  if (changed) renderTabs();
}

// renderList mirrors the Mac sidebar: workspaces grouped under their window's
// name (shared via ws.group — the Mac app pushes it), sorted by name within
// each group, each row carrying the full git dashboard.
function renderList() {
  const ul = $("ws-list");
  ul.innerHTML = "";
  for (const [group, list] of groupedWorkspaces()) {
    if (group) {
      const h = document.createElement("li");
      h.className = "group-hdr";
      h.innerHTML = `<span>${esc(group.toUpperCase())}</span>` +
        `<button class="grp-msgs" title="Peer messages in ${esc(group)}">💬</button>`;
      h.querySelector(".grp-msgs").onclick = (e) => {
        e.stopPropagation();
        window.ccmuxPeers.open(group);
      };
      ul.appendChild(h);
    }
    for (const ws of list) ul.appendChild(wsRow(ws));
  }
}

// groupedWorkspaces buckets by shared group: ungrouped ("") first with no
// header (daemon-only deployments where no Mac has pushed groups), then named
// groups alphabetically; workspaces sort by name inside each.
function groupedWorkspaces() {
  const byGroup = new Map();
  for (const ws of state.workspaces) {
    const g = ws.group || "";
    if (!byGroup.has(g)) byGroup.set(g, []);
    byGroup.get(g).push(ws);
  }
  for (const list of byGroup.values()) {
    list.sort((a, b) => (a.name || "").localeCompare(b.name || "", undefined, { sensitivity: "base" }));
  }
  return [...byGroup.entries()].sort(([a], [b]) =>
    a === "" ? -1 : b === "" ? 1 : a.localeCompare(b));
}

function wsRow(ws) {
  const active = ws.id === state.wsId;
  // Suppress the flash on the workspace you're already watching (mirrors the
  // native "clear on watch"); other rows flash live from the firehose.
  const att = active ? "" : wsAttention(ws);
  const open = !!state.gitOpen[ws.id];
  const running = (ws.panes || []).some((p) => p.attention === "running");
  const cold = ws.status === "cold";
  const li = document.createElement("li");
  li.className = "ws" + (active ? " active" : "") + (att ? " att-" + att : "") + (cold ? " cold" : "");
  li.innerHTML =
    `<div class="ws-row">` +
    `<span class="exp${open ? " open" : " closed"}"></span>` +
    `<span class="dot ${esc(ws.status)}"></span>` +
    `<span class="name">${esc(ws.name || ws.repoPath)}</span>` +
    (running ? `<span class="bolt">⚡</span>` : "") +
    (cold ? `<span class="cold-tag">zzz</span>` : gitBadges(ws.git)) +
    `<button class="more" title="Session menu">⋯</button>` +
    `</div>` +
    (open ? gitDetail(ws) : "");
  // A cold session has nothing to attach to — clicking revives it in place.
  li.onclick = cold ? () => reviveWorkspace(ws.id) : () => attach(ws.id, null);
  li.querySelector(".exp").onclick = (e) => {
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
  if (ws.status === "cold") {
    add("Revive", () => reviveWorkspace(ws.id));
  } else {
    add("Hostnames…", () => openHostnamesModal(ws));
    const hostnames = ws.hostnames || [];
    if (hostnames.length) {
      const running = (ws.panes || []).some((p) => p.devServer);
      add(running ? "Stop Dev Server" : "Start Dev Server", () => setDevServer(ws.id, !running));
    }
    for (const h of hostnames.filter((h) => h.url)) {
      add(`${h.listening ? "●" : "○"} ${h.name} : ${h.port}`, () => window.open(h.url, "_blank"));
    }
    sep();
    add("Close Session", () => closeSession(ws.id));
  }
  add("Remove Session…", () => removeSession(ws), "danger");

  // Show first so it has a size, then clamp into the viewport.
  menu.classList.remove("hidden");
  const r = menu.getBoundingClientRect();
  menu.style.left = Math.max(4, Math.min(x, window.innerWidth - r.width - 8)) + "px";
  menu.style.top = Math.max(4, Math.min(y, window.innerHeight - r.height - 8)) + "px";
}

function closeWsMenu() { $("ctx-menu").classList.add("hidden"); }

async function setDevServer(id, start) {
  const r = await fetch(`/v1/workspaces/${id}/dev-server`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ action: start ? "start" : "stop" }),
  });
  if (!r.ok) alert("dev server: " + (await r.text()));
  fetchWorkspaces();
}

// --- hostnames editor: {name, port} rows + the ▶ dev command. An empty sheet
// prefills from the daemon's repo-config detection, like the Mac sheet. Save
// PUTs and closes only on success; daemon validation errors stay inline. ---
let hostnamesWsId = null;

async function openHostnamesModal(ws) {
  hostnamesWsId = ws.id;
  $("hostnames-title").textContent = `Hostnames — ${ws.name}`;
  $("hostnames-error").classList.add("hidden");
  const rows = $("hostnames-rows");
  rows.innerHTML = "";
  let mappings = (ws.hostnames || []).map((h) => ({ name: h.name, port: h.port }));
  let cmd = ws.devCommand || "";
  if (!mappings.length || !cmd) {
    try {
      const s = await (await fetch(`/v1/workspaces/${ws.id}/port-suggestions`)).json();
      if (!mappings.length) mappings = (s.suggestions || []).map((x) => ({ name: x.name, port: x.port }));
      if (!cmd && s.devCommand) $("hostnames-cmd").placeholder = `dev command — detected: ${s.devCommand}`;
    } catch (_) { /* suggestions are best-effort */ }
  }
  if (!mappings.length) mappings = [{ name: "", port: "" }];
  for (const m of mappings) rows.appendChild(hostnameRow(m.name, m.port));
  $("hostnames-cmd").value = cmd;
  $("hostnames-modal").classList.remove("hidden");
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
  const hostnames = [...$("hostnames-rows").children]
    .map((li) => ({
      name: li.querySelector(".hn-name").value.trim(),
      port: parseInt(li.querySelector(".hn-port").value, 10) || 0,
    }))
    .filter((h) => h.name || h.port);
  const body = { hostnames, devCommand: $("hostnames-cmd").value.trim() };
  const r = await fetch(`/v1/workspaces/${hostnamesWsId}/hostnames`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!r.ok) {
    let msg = "HTTP " + r.status;
    try { msg = (await r.json()).error || msg; } catch (_) {}
    const err = $("hostnames-error");
    err.textContent = msg;
    err.classList.remove("hidden");
    return;
  }
  $("hostnames-modal").classList.add("hidden");
  fetchWorkspaces();
}

async function reviveWorkspace(id) {
  const r = await fetch(`/v1/workspaces/${id}/revive`, { method: "POST" });
  if (!r.ok) { alert("revive failed: " + (await r.text())); return; }
  await fetchWorkspaces();
  attach(id, null);
}

async function closeSession(id) {
  const r = await fetch(`/v1/workspaces/${id}/archive`, { method: "POST" });
  if (!r.ok) { alert("close failed: " + (await r.text())); return; }
  detachIfCurrent(id);
  fetchWorkspaces();
}

async function removeSession(ws) {
  const sure = confirm(
    `Remove “${ws.name}”?\n\nThis kills the session and permanently deletes its ` +
    `panes, layout, hostnames, and dev command. Use “Close session” instead to ` +
    `keep them for a later revive.`);
  if (!sure) return;
  const r = await fetch(`/v1/workspaces/${ws.id}`, { method: "DELETE" });
  if (!r.ok) { alert("remove failed: " + (await r.text())); return; }
  detachIfCurrent(ws.id);
  fetchWorkspaces();
}

// detachIfCurrent drops the attach when the workspace we're watching goes away
// (closed/removed) — back to the empty state, so re-surface the session list.
function detachIfCurrent(id) {
  if (state.wsId !== id) return;
  if (state.conn) { state.conn.close(); state.conn = null; }
  state.wsId = null;
  state.panes = [];
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
function wsAttention(ws) {
  const per = state.attn[ws.id];
  if (!per) return "";
  let done = false;
  for (const p of ws.panes || []) {
    if (per[p.id] === "needs_input") return "needs_input";
    if (per[p.id] === "done") done = true;
  }
  return done ? "done" : "";
}

// --- terminal ---
function ensureTerm() {
  if (state.term) return;
  state.term = new Terminal({
    fontSize: 13,
    fontFamily: 'Menlo, Monaco, "SF Mono", monospace',
    cursorBlink: true,
    scrollback: 5000,
    allowProposedApi: true,
    theme: { background: "#1c1e22", foreground: "#d9dbdd", cursor: "#d9dbdd" },
  });
  state.fit = new FitAddon.FitAddon();
  state.term.loadAddon(state.fit);
  state.term.open($("terminal"));
  state.term.onData((d) => sendInput(d));
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

function reportFocus() {
  const watching = document.visibilityState === "visible" && document.hasFocus();
  send({ t: "focus", pane: watching ? state.paneId : "" });
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

function doFit() {
  if (!state.fit || !state.term) return;
  try { state.fit.fit(); } catch (_) {}
  const { cols, rows } = state.term;
  if (!cols || !rows || cols < MIN_COLS) return;
  send({ t: "resize", pane: state.paneId, cols, rows });
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
async function attach(wsId, wantPane) {
  closeDrawer(); // selecting a session on mobile dismisses the drawer
  state.paneCols = 0;
  if (state.conn) { state.conn.close(); state.conn = null; }
  state.wsId = wsId;
  state.wantPane = wantPane;
  delete state.attn[wsId]; // opening it marks its attention seen
  $("empty").style.display = "none";
  ensureTerm();
  state.term.reset();
  renderList();

  // If the workspace names a host we don't know yet (joined after boot), refresh
  // the registry so we can dial it directly rather than mis-routing to the hub.
  const ws = state.workspaces.find((w) => w.id === wsId);
  if (ws && ws.host && !state.hosts[ws.host]) await fetchHosts();

  const q = `workspace=${wsId}&user=${encodeURIComponent(getUser())}&device=web`;
  const conn = new WebSocket(`${attachOrigin(ws)}/v1/attach?${q}`);
  conn.onmessage = onMessage;
  conn.onopen = () => scheduleFit();
  state.conn = conn;
}

function onMessage(ev) {
  const m = JSON.parse(ev.data);
  switch (m.t) {
    case "hello":
      state.panes = m.panes || [];
      state.paneId = state.wantPane || (state.panes[0] && state.panes[0].id) || null;
      state.wantPane = null;
      state.paneCols = paneColsOf(state.paneId);
      renderTabs();
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

// --- pane tabs ---
function renderTabs() {
  const tabs = $("tabs");
  tabs.innerHTML = "";
  state.panes.forEach((p, i) => {
    const b = document.createElement("button");
    b.className = "tab" + (p.id === state.paneId ? " active" : "") + " att-" + (p.attention || "idle");
    b.dataset.pane = p.id;
    b.textContent = p.title || `pane ${i + 1}`;
    b.onclick = () => attach(state.wsId, p.id);
    tabs.appendChild(b);
  });
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
  const fh = new WebSocket(`${proto}://${location.host}/v1/events`);
  fh.onmessage = onFirehose;
  fh.onclose = () => { state.firehose = null; setTimeout(connectFirehose, 2000); };
  state.firehose = fh;
}

function onFirehose(ev) {
  let m;
  try { m = JSON.parse(ev.data); } catch (_) { return; }
  if (m.t === "hello") {
    state.attn = {};
    for (const e of m.attention || []) noteAttention(e.workspace, e.pane, e.state);
  } else if (m.t === "attention") {
    noteAttention(m.workspace, m.pane, m.state);
  } else if (m.t === "workspace-added" || m.t === "workspace-removed" || m.t === "workspace-status" || m.t === "workspace-git") {
    fetchWorkspaces(); // a workspace changed elsewhere — refresh now, don't wait for the poll
    return;
  } else {
    return;
  }
  renderList();
}

function noteAttention(wsId, paneId, stateStr) {
  if (!wsId || !paneId) return;
  (state.attn[wsId] || (state.attn[wsId] = {}))[paneId] = stateStr;
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
function projectsURL(relPath) {
  const host = createHostId();
  const base = host ? `/v1/hosts/${encodeURIComponent(host)}/projects` : "/v1/projects";
  return base + "?path=" + encodeURIComponent(relPath);
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
  // Show what a workspace would run by default, as the override placeholder.
  fetch("/v1/settings").then((r) => r.json()).then((cfg) => {
    $("project-cmd").placeholder = `startup command — empty = default (${cfg.startupCommand || "shell"})`;
  }).catch(() => {});
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

async function createWorkspace(p) {
  // startupCommand omitted = the daemon resolves it (per-folder rules, then
  // the Settings default); the picker's field is a one-off override.
  const body = { name: p.name, repoPath: p.path, createdBy: "web" };
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
  attach(ws.id, null);
}

// --- settings: the daemon-wide startup command + per-folder rules for new
// hosted workspaces. Loaded when the settings sheet opens; saved on change.
// addEventListener (not onclick) so push.js's own open handler coexists. ---
function wireStartupCommandSetting() {
  const input = $("startup-cmd"), state = $("startup-cmd-state");
  const rulesBox = $("startup-rules"), addBtn = $("startup-rule-add");
  if (!input) return;

  function ruleRow(rule) {
    const row = document.createElement("div");
    row.className = "rule-row";
    row.innerHTML =
      `<input class="setting-input rule-prefix" type="text" spellcheck="false" placeholder="/path/to/folder" value="${esc(rule.pathPrefix || "")}">` +
      `<input class="setting-input rule-cmd" type="text" spellcheck="false" placeholder="command" value="${esc(rule.command || "")}">` +
      `<button class="rule-del" type="button" title="Remove rule">&times;</button>`;
    for (const el of row.querySelectorAll("input")) {
      el.addEventListener("change", saveRules);
      el.addEventListener("keydown", (e) => { if (e.key === "Enter") el.blur(); });
    }
    row.querySelector(".rule-del").onclick = () => { row.remove(); saveRules(); };
    return row;
  }

  function collectRules() {
    return [...rulesBox.querySelectorAll(".rule-row")].map((row) => ({
      pathPrefix: row.querySelector(".rule-prefix").value.trim(),
      command: row.querySelector(".rule-cmd").value.trim(),
    }));
  }

  async function load() {
    try {
      const r = await fetch("/v1/settings");
      const cfg = await r.json();
      input.value = cfg.startupCommand || "";
      rulesBox.innerHTML = "";
      for (const rule of cfg.startupRules || []) rulesBox.appendChild(ruleRow(rule));
      state.textContent = "Typed into every new hosted workspace's terminal (all lenses).";
    } catch (_) {
      state.textContent = "Couldn't load the current settings.";
    }
  }

  async function put(body) {
    const r = await fetch("/v1/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    return r.json();
  }

  async function saveCommand() {
    try {
      const cfg = await put({ startupCommand: input.value });
      input.value = cfg.startupCommand || ""; // empty resets to built-in — show it
      state.textContent = "Saved.";
    } catch (_) {
      state.textContent = "Couldn't save — is the daemon reachable?";
    }
  }

  async function saveRules() {
    try {
      await put({ startupRules: collectRules() });
      // Don't re-render: half-filled rows stay editable (the daemon drops them).
      state.textContent = "Saved.";
    } catch (_) {
      state.textContent = "Couldn't save — is the daemon reachable?";
    }
  }

  $("open-settings").addEventListener("click", load);
  input.addEventListener("change", saveCommand);
  input.addEventListener("keydown", (e) => { if (e.key === "Enter") input.blur(); });
  addBtn.addEventListener("click", () => {
    rulesBox.appendChild(ruleRow({ pathPrefix: "", command: "" }));
    rulesBox.lastChild.querySelector(".rule-prefix").focus();
  });
}
wireStartupCommandSetting();

// Opened from a notification tap (/?ws=<id>): attach straight to that workspace.
// Without a deep link there's nothing on screen but "Select a workspace", so
// surface the session list (the flyout drawer on mobile) instead of a dead end.
function bootDeepLink() {
  const ws = new URLSearchParams(location.search).get("ws");
  if (ws) attach(ws, null);
  else openDrawer();
}

// --- boot ---
window.ccmux = { attach, getUser }; // push.js deep-links + shares the presence name
$("new-ws").onclick = newWorkspace;
$("project-close").onclick = closeProjectModal;
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
fetchHosts().then(fetchWorkspaces).then(bootDeepLink); // hosts first so deep-link attach dials direct
connectFirehose();
setInterval(fetchWorkspaces, 5000); // reflect status/pane-count changes
