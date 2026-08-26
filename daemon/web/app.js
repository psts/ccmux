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
  paneCols: 0,       // authoritative width of the current pane (from the daemon)
  gitOpen: {},       // wsId -> true when the row's changed-files list is expanded
};

const $ = (id) => document.getElementById(id);
// Quotes included: group names are user-typed (moveToWindow's prompt) and get
// interpolated into title="..." attributes — a bare " would break out.
const esc = (s) => String(s).replace(/[<>&"']/g,
  (c) => ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&#39;" }[c]));

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
    const [wr, winr] = await Promise.all([fetch("/v1/workspaces"), fetch("/v1/windows")]);
    state.workspaces = (await wr.json()) || [];
    // Keep the last window list on a failed read (503 = tables unreadable,
    // per the daemon) — blanking it would make every window look closed and
    // feed wrong close decisions. Same fallback the Mac lens uses.
    if (winr.ok) state.windows = (await winr.json()) || [];
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
  let changed = false, shellChanged = false;
  for (const p of state.panes) {
    const q = ws.panes.find((x) => x.id === p.id);
    if (!q) continue;
    if (q.title !== p.title) { p.title = q.title; changed = true; }
    // atShell/harness arrive late for a fresh pane (the attach hello races
    // tmux's first command signal), so the harness bar re-derives from every
    // registry refresh — the firehose fires one for exactly this change.
    if (!!q.atShell !== !!p.atShell || !!q.dormant !== !!p.dormant ||
        !!q.devServer !== !!p.devServer || (q.harness || "") !== (p.harness || "")) {
      p.atShell = q.atShell;
      p.dormant = q.dormant;
      p.devServer = q.devServer;
      p.harness = q.harness;
      shellChanged = true;
    }
  }
  if (changed || shellChanged) renderTabs(); // dormant styling rides the tabs
  if (shellChanged) updateHarnessBar();
}

// renderList renders the SHARED arrangement (v2): windows you have OPEN as
// sections, windows you have closed as one-line rows you can open, and
// ungrouped sessions under AVAILABLE. The arrangement is the same for
// everyone; open/closed is yours.
function renderList() {
  const ul = $("ws-list");
  ul.innerHTML = "";
  const grouped = groupedWorkspaces();
  const winByName = new Map(state.windows.map((w) => [w.name.toLowerCase(), w]));
  const closed = state.windows.filter((w) => !w.open);

  for (const [group, list] of grouped) {
    if (group) {
      const win = winByName.get(group.toLowerCase());
      if (win && !win.open) continue; // rendered below as a closed-window row
      const h = document.createElement("li");
      h.className = "group-hdr";
      h.innerHTML = `<span>${esc(group.toUpperCase())}</span>` +
        (win ? `<button class="grp-close" title="Close window">–</button>` : "") +
        `<button class="grp-msgs" title="Peer messages in ${esc(group)}">💬</button>`;
      h.querySelector(".grp-msgs").onclick = (e) => {
        e.stopPropagation();
        window.ccmuxPeers.open(group);
      };
      if (win) {
        h.querySelector(".grp-close").onclick = (e) => {
          e.stopPropagation();
          closeWindow(win);
        };
      }
      ul.appendChild(h);
    } else if (grouped.length > 1 || closed.length) {
      // Header only when there is something to separate from: a deployment
      // where nothing is grouped keeps its plain flat list.
      const h = document.createElement("li");
      h.className = "group-hdr";
      h.innerHTML = "<span>AVAILABLE</span>";
      ul.appendChild(h);
    }
    for (const ws of list) ul.appendChild(wsRow(ws));
  }

  if (closed.length) {
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
      li.onclick = () => openWindow(win);
      ul.appendChild(li);
    }
  }
}

// groupedWorkspaces buckets by the shared window name: named windows
// alphabetically, the ungrouped bucket ("") last; workspaces sort by name.
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
    a === "" ? 1 : b === "" ? -1 : a.localeCompare(b));
}

// ownerLabel says whose session an ungrouped row is: "patric". Empty for
// grouped rows (the window is shared; whose host it runs on is a detail).
function ownerLabel(ws) {
  if (ws.group || !ws.owner) return "";
  return (ws.owner || "").split("@")[0];
}

// openWindow marks the shared window open for this login and wakes its cold
// members — opening a sleeping window is how it comes back everywhere.
async function openWindow(win) {
  try {
    const r = await fetch(`/v1/windows/${win.id}/open`, { method: "POST" });
    if (!r.ok) { alert("open failed: " + (await r.text())); return; }
    for (const wsId of win.workspaceIds || []) {
      const ws = state.workspaces.find((w) => w.id === wsId);
      if (ws && ws.status === "cold") {
        const rr = await fetch(`/v1/workspaces/${wsId}/revive`, { method: "POST" });
        if (!rr.ok) alert(`could not wake ${ws.name || wsId}: ` + (await rr.text()));
      }
    }
  } catch (e) {
    alert("open failed: " + e.message);
    return;
  }
  fetchWorkspaces();
}

// closeWindow clears this login's open flag; when that was the LAST opener,
// the window goes to sleep — its members archive (force: nobody has it open,
// which is the model's own permission).
async function closeWindow(win) {
  try {
    const r = await fetch(`/v1/windows/${win.id}/close`, { method: "POST" });
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
  fetchWorkspaces();
}

function wsRow(ws) {
  const active = ws.id === state.wsId;
  // Suppress the flash on the workspace you're already watching (mirrors the
  // native "clear on watch"); other rows flash live from the firehose.
  const att = active ? "" : wsAttention(ws);
  const open = !!state.gitOpen[ws.id];
  const running = (ws.panes || []).some((p) => p.attention === "running");
  const cold = ws.status === "cold";
  const label = ownerLabel(ws);
  const li = document.createElement("li");
  li.className = "ws" + (active ? " active" : "") + (att ? " att-" + att : "") + (cold ? " cold" : "");
  li.innerHTML =
    `<div class="ws-row">` +
    `<span class="exp${open ? " open" : " closed"}"></span>` +
    `<span class="dot ${esc(ws.status)}"></span>` +
    `<span class="name">${esc(ws.name || ws.repoPath)}</span>` +
    (label ? `<span class="owner-tag">${esc(label)}</span>` : "") +
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
  // Windows are SHARED (v2): placing, moving, and removing change the one
  // arrangement everyone sees.
  add(ws.group ? "Move to Window…" : "Add to Window…", () => moveToWindow(ws));
  if (ws.group) add("Remove from Window (for everyone)", () => putGroup(ws.id, ""));
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

// Focus and presence are two different claims. Focus says which pane this lens is
// watching, and clears that workspace's flash. Presence says this screen could
// show a notification at all — for a browser, that it is visible rather than
// buried or backgrounded. A visible-but-unfocused tab still counts as present:
// the person is at the machine, which is what the daemon needs to know before it
// decides whether to alert here or buzz their phone.
function reportFocus() {
  const visible = document.visibilityState === "visible";
  const watching = visible && document.hasFocus();
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
  $("harness-bar").classList.add("hidden"); // re-derived from the next hello
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
      state.paneId = state.wantPane || (state.panes[0] && state.panes[0].id) || null;
      state.wantPane = null;
      state.paneCols = paneColsOf(state.paneId);
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

// --- pane tabs ---
function renderTabs() {
  const tabs = $("tabs");
  tabs.innerHTML = "";
  state.panes.forEach((p, i) => {
    const b = document.createElement("button");
    // A dormant pane hosted a Claude session that has exited. The pane is alive,
    // which is exactly why it needs saying — nothing else tells it apart from a
    // working session.
    b.className = "tab" + (p.id === state.paneId ? " active" : "") +
      " att-" + (p.attention || "idle") + (p.dormant ? " dormant" : "");
    b.dataset.pane = p.id;
    b.title = p.dormant ? "Claude exited — shell only" : "";
    b.textContent = p.title || `pane ${i + 1}`;
    b.onclick = () => attach(state.wsId, p.id);
    b.oncontextmenu = (e) => { e.preventDefault(); openPaneLLMMenu(p.id, e.clientX, e.clientY); };
    tabs.appendChild(b);
  });
}

// Right-click on a pane tab: pick which LLM account answers THIS pane. The
// choice applies to the pane's next request — no restart. "Global default"
// clears the override so the pane follows the settings-modal route again.
async function openPaneLLMMenu(paneId, x, y) {
  let cur;
  try {
    const r = await fetch(`/v1/panes/${paneId}/llm-route`);
    if (!r.ok) return; // proxy not mounted or pane unknown — no menu to offer
    cur = await r.json();
  } catch (_) { return; }

  const menu = $("ctx-menu");
  menu.innerHTML = "";
  const line = document.createElement("div");
  line.className = "host-line";
  line.textContent = "LLM route · now: " + cur.effective;
  menu.appendChild(line);

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
  add("Global default", "", cur.route === "");
  for (const name of cur.accounts || []) add(name, name, cur.route === name);

  menu.classList.remove("hidden");
  const r = menu.getBoundingClientRect();
  menu.style.left = Math.max(4, Math.min(x, window.innerWidth - r.width - 8)) + "px";
  menu.style.top = Math.max(4, Math.min(y, window.innerHeight - r.height - 8)) + "px";
}

// settingsURLFor targets the settings of the host that owns ws (the hub
// proxies member hosts), with the repoPath so the response carries the
// folder rule's preselection.
function settingsURLFor(ws) {
  const base = ws && ws.host ? `/v1/hosts/${encodeURIComponent(ws.host)}/settings` : "/v1/settings";
  return ws && ws.repoPath ? `${base}?repoPath=${encodeURIComponent(ws.repoPath)}` : base;
}

// --- harness bar: shown when the ACTIVE pane sits at a bare shell with no
// harness recorded — the empty-new-workspace flow, and any shell tab. The
// folder rule only PRESELECTS (highlighted); nothing ever auto-starts. ---
async function updateHarnessBar() {
  const bar = $("harness-bar");
  const paneId = state.paneId;
  const p = state.panes.find((x) => x.id === paneId);
  // A pane at a bare shell gets the bar: no harness yet = "Start here:",
  // harness recorded but exited (its shell is back) = "Restart:".
  if (!p || p.devServer || !(p.atShell || p.dormant)) { setHarnessBar(false); return; }
  const ws = state.workspaces.find((w) => w.id === state.wsId);
  let cfg;
  try {
    cfg = await (await fetch(settingsURLFor(ws))).json();
  } catch (_) { setHarnessBar(false); return; }
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
  attach(state.wsId, p.id); // land on the pane just asked for
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
  const r = await fetch(projectsBase(), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ path: rel, git: $("project-folder-git").checked }),
  });
  if (!r.ok) { alert("create folder: " + (await r.text())); return; }
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
  attach(ws.id, null);
}

// --- settings: LLM routing. Accounts are places pane LLM traffic can go
// (Ollama, OpenRouter, a keyed Anthropic org); the route picks which one
// answers right now. Empty route = direct Anthropic pass-through (the Max
// OAuth default). Keys are write-only: an account saved with an empty key
// keeps its stored one, so re-saving this form never wipes a secret. ---
function wireLLMSettings() {
  const routeSel = $("llm-route"), box = $("llm-accounts");
  const addBtn = $("llm-account-add"), statusEl = $("llm-state");
  const routeStateEl = $("llm-route-state") || statusEl;
  if (!routeSel) return;

  // One line of live health under an account: reachable, limited (and until
  // when), token rejected — plus usage percentages where the upstream sends
  // them (Anthropic subscriptions: session = 5h window, week = 7 days).
  function statusLine(st) {
    if (!st) return "";
    const when = (iso) => iso ? new Date(iso).toLocaleString([], { weekday: "short", hour: "2-digit", minute: "2-digit" }) : "";
    let s = { ok: "● active", limited: "◐ limited", unauthorized: "✕ credential rejected", untried: "○ no traffic yet" }[st.state] || st.state;
    if (st.state === "limited" && st.limitedUntil) s += " until " + when(st.limitedUntil);
    const usage = [];
    if (st.sessionPct >= 0) usage.push(`session ${st.sessionPct}%${st.sessionReset ? " (resets " + when(st.sessionReset) + ")" : ""}`);
    if (st.weeklyPct >= 0) usage.push(`week ${st.weeklyPct}%`);
    if (usage.length) s += " · " + usage.join(" · ");
    return s;
  }

  function accountRow(a, st) {
    const row = document.createElement("div");
    row.className = "entry-card";
    const keyHint = a.apiKeySet ? "set — empty keeps it" : (a.kind === "claude" ? "paste `claude setup-token` output" : "empty = your own login");
    const aliases = (a.modelAliases || []).map((x) => `${x.from}=${x.to}`).join(", ");
    // What the picker shows as its current choice: the claude-* rule's target.
    const claudeTarget = ((a.modelAliases || []).find((x) => x.from === "claude-*") || {}).to || "";
    const status = statusLine(st);
    row.innerHTML =
      `<div class="entry-line">` +
      `<input class="setting-input llm-name grow" type="text" spellcheck="false" placeholder="name" value="${esc(a.name || "")}">` +
      `<select class="setting-input llm-kind">` +
      ["anthropic", "openai", "claude", "codex"].map((k) =>
        `<option value="${k}"${(a.kind || "anthropic") === k ? " selected" : ""}>${k}</option>`).join("") +
      `</select>` +
      `<button class="rule-del" type="button" title="Remove account">&times;</button>` +
      `</div>` +
      `<div class="entry-line">` +
      `<input class="setting-input llm-url grow" type="text" spellcheck="false" placeholder="base URL, e.g. http://localhost:11434" value="${esc(a.baseURL || "")}">` +
      `</div>` +
      `<div class="entry-line">` +
      `<input class="setting-input llm-key grow" type="password" autocomplete="off" placeholder="token / api key: ${esc(keyHint)}">` +
      `<input class="setting-input llm-aliases grow" type="text" spellcheck="false" placeholder="aliases: claude-haiku-*=qwen3-4b-32k" value="${esc(aliases)}">` +
      `<select class="setting-input llm-model-pick" title="List the upstream's models; picking one maps every claude-* request to it">` +
      `<option value="">map claude → …</option>` +
      (claudeTarget ? `<option value="${esc(claudeTarget)}" selected>${esc(claudeTarget)}</option>` : "") +
      `</select>` +
      `</div>` +
      (status ? `<div class="entry-line llm-acct-status">${esc(status)}</div>` : "");
    for (const el of row.querySelectorAll("input, select:not(.llm-model-pick)")) {
      el.addEventListener("change", saveAccounts);
      el.addEventListener("keydown", (e) => { if (e.key === "Enter") el.blur(); });
    }
    // Picking a model rewrites the alias field (claude-* rules replaced,
    // custom rules kept) and saves; the picker keeps showing the choice.
    row.querySelector(".llm-model-pick").addEventListener("change", (e) => {
      const model = e.target.value;
      if (!model) return;
      const field = row.querySelector(".llm-aliases");
      const kept = parseAliases(field.value).filter((x) => !x.from.startsWith("claude-"));
      field.value = kept.concat([{ from: "claude-*", to: model }])
        .map((x) => `${x.from}=${x.to}`).join(", ");
      saveAccounts();
    });
    row.querySelector(".rule-del").onclick = () => { row.remove(); saveAccounts(); };
    return row;
  }

  // Fill each SAVED account's model picker from its upstream's /v1/models
  // (Ollama's list of pulled models, OpenRouter's catalog, …). An upstream
  // that doesn't answer just leaves that picker with its placeholder.
  async function populateModelPicks() {
    for (const row of box.querySelectorAll(".entry-card")) {
      const name = row.querySelector(".llm-name").value.trim();
      const sel = row.querySelector(".llm-model-pick");
      if (!name || !sel) continue;
      try {
        const r = await fetch(`/v1/llm/accounts/${encodeURIComponent(name)}/models`);
        if (!r.ok) {
          // The backend reports WHY (upstream down, bad key, wrong URL) —
          // an empty picker that hides the reason reads as a broken feature.
          const msg = (await r.json().catch(() => ({}))).error || `HTTP ${r.status}`;
          sel.title = "couldn't list models: " + msg;
          sel.options[0].textContent = "models unavailable";
          continue;
        }
        const have = new Set([...sel.options].map((o) => o.value));
        for (const m of (await r.json()).models || []) {
          if (have.has(m)) continue; // the current mapping is already an option
          const o = document.createElement("option");
          o.value = m;
          o.textContent = m;
          sel.appendChild(o);
        }
      } catch (_) { /* placeholder stays */ }
    }
  }

  // "from=to, from2=to2" — rows without an '=' are dropped as half-typed.
  function parseAliases(text) {
    return text.split(",").map((s) => s.trim()).filter((s) => s.includes("=")).map((s) => {
      const i = s.indexOf("=");
      return { from: s.slice(0, i).trim(), to: s.slice(i + 1).trim() };
    }).filter((x) => x.from && x.to);
  }

  function collectAccounts() {
    return [...box.querySelectorAll(".entry-card")].map((row) => ({
      name: row.querySelector(".llm-name").value.trim(),
      kind: row.querySelector(".llm-kind").value,
      baseURL: row.querySelector(".llm-url").value.trim(),
      apiKey: row.querySelector(".llm-key").value, // empty keeps the stored key
      modelAliases: parseAliases(row.querySelector(".llm-aliases").value),
    })).filter((a) => a.name || a.baseURL); // a fully blank editor row isn't an account
  }

  function renderRoute(accounts, route) {
    routeSel.innerHTML = "";
    const direct = document.createElement("option");
    direct.value = "";
    direct.textContent = "Anthropic (direct, your Claude login)";
    routeSel.appendChild(direct);
    for (const a of accounts) {
      const o = document.createElement("option");
      o.value = a.name;
      o.textContent = a.name + (a.baseURL ? "  →  " + a.baseURL : "");
      routeSel.appendChild(o);
    }
    routeSel.value = route || "";
  }

  async function put(body) {
    const r = await fetch("/v1/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  }

  async function load() {
    try {
      const cfg = await (await fetch("/v1/settings")).json();
      const stByName = {};
      for (const st of cfg.llmAccountStatus || []) stByName[st.name] = st;
      box.innerHTML = "";
      for (const a of cfg.llmAccounts || []) box.appendChild(accountRow(a, stByName[a.name]));
      renderRoute(cfg.llmAccounts || [], cfg.llmRoute);
      populateModelPicks(); // fire-and-forget: pickers fill as upstreams answer
      statusEl.textContent = "Applies to every pane's next request — no restarts.";
    } catch (_) {
      statusEl.textContent = "Couldn't load LLM settings.";
    }
  }

  async function saveAccounts() {
    try {
      const cfg = await put({ llmAccounts: collectAccounts() });
      renderRoute(cfg.llmAccounts || [], cfg.llmRoute);
      statusEl.textContent = "Saved.";
    } catch (e) {
      statusEl.textContent = "Not saved: " + e.message;
    }
  }

  $("open-settings").addEventListener("click", load);
  routeSel.addEventListener("change", async () => {
    try {
      await put({ llmRoute: routeSel.value });
      routeStateEl.textContent = routeSel.value
        ? `Routing all panes to ${routeSel.value}.` : "Routing direct to Anthropic.";
    } catch (e) {
      routeStateEl.textContent = "Not saved: " + e.message;
      load(); // the picker now lies — reload truth
    }
  });
  addBtn.addEventListener("click", () => {
    box.appendChild(accountRow({}));
    box.lastChild.querySelector(".llm-name").focus();
  });
}
wireLLMSettings();

// --- settings: per-folder harness rules — which harness a new workspace
// under a folder PRESELECTS on its harness bar (a suggestion, nothing
// auto-starts; longest matching folder wins). A sub-editor of the Harnesses
// tab: wireHarnessSettings owns the settings fetch and calls render() with
// the current harness names, both on load and after a harness save (renames
// and deletes change what a rule can point at). ---
function wireHarnessRules(put) {
  const rulesBox = $("harness-rules"), addBtn = $("harness-rule-add"), statusEl = $("harness-rules-state");
  let harnessNames = [];

  // A rule names a harness, so its select is built from the loaded list; a
  // rule whose harness no longer exists keeps a disabled option so the row
  // stays visible and deletable instead of silently jumping to another name.
  function ruleRow(rule) {
    const row = document.createElement("div");
    row.className = "rule-row";
    const options = harnessNames.map((n) =>
      `<option value="${esc(n)}"${n === rule.harness ? " selected" : ""}>${esc(n)}</option>`);
    if (rule.harness && !harnessNames.includes(rule.harness)) {
      options.push(`<option value="${esc(rule.harness)}" selected disabled>${esc(rule.harness)} (gone)</option>`);
    }
    row.innerHTML =
      `<input class="setting-input rule-prefix" type="text" spellcheck="false" placeholder="/path/to/folder" value="${esc(rule.pathPrefix || "")}">` +
      `<select class="setting-input rule-harness">${options.join("")}</select>` +
      `<button class="rule-del" type="button" title="Remove rule">&times;</button>`;
    const prefix = row.querySelector(".rule-prefix");
    prefix.addEventListener("change", saveRules);
    prefix.addEventListener("keydown", (e) => { if (e.key === "Enter") prefix.blur(); });
    row.querySelector(".rule-harness").addEventListener("change", saveRules);
    row.querySelector(".rule-del").onclick = () => { row.remove(); saveRules(); };
    return row;
  }

  function collectRules() {
    return [...rulesBox.querySelectorAll(".rule-row")].map((row) => ({
      pathPrefix: row.querySelector(".rule-prefix").value.trim(),
      harness: row.querySelector(".rule-harness").value,
    }));
  }

  function render(names, rules) {
    harnessNames = names;
    rulesBox.innerHTML = "";
    for (const rule of rules) rulesBox.appendChild(ruleRow(rule));
  }

  async function saveRules() {
    try {
      await put({ harnessRules: collectRules() });
      // Don't re-render: half-filled rows stay editable (the daemon drops them).
      statusEl.textContent = "Saved.";
    } catch (e) {
      statusEl.textContent = "Not saved: " + e.message;
    }
  }

  addBtn.addEventListener("click", () => {
    rulesBox.appendChild(ruleRow({ pathPrefix: "", harness: harnessNames[0] || "claude" }));
    rulesBox.lastChild.querySelector(".rule-prefix").focus();
  });

  return { render, collect: collectRules };
}

// --- settings: harnesses — the single source of what a pane runs. The daemon
// lists claude (builtin) and every known program it finds installed
// (detected) with zero config; editing one of those rows saves a user
// OVERRIDE by name, and only overrides and new entries are persisted — an
// untouched builtin/detected row stays live-resolved (a detected harness
// disappears when uninstalled). Deleting an override restores the default.
// The command field is where per-harness flags live, e.g. claude's
// --dangerously-load-development-channels. ---
function wireHarnessSettings() {
  const box = $("harness-list"), addBtn = $("harness-add"), statusEl = $("harness-state");
  if (!box) return;

  function harnessRow(h) {
    const row = document.createElement("div");
    row.className = "entry-card";
    const kindsText = (h.accountKinds || []).join(", ");
    row.dataset.orig = JSON.stringify({ icon: h.icon || "", name: h.name || "", command: h.command || "", autoconfirm: !!h.autoconfirm, kinds: kindsText });
    row.dataset.source = h.source || "";
    const badge = h.source ? `<span class="harness-src">${esc(h.source)}</span>` : "";
    row.innerHTML =
      `<div class="entry-line">` +
      `<input class="setting-input hx-icon" type="text" spellcheck="false" placeholder="✳" value="${esc(h.icon || "")}">` +
      `<input class="setting-input hx-name grow" type="text" spellcheck="false" placeholder="name" value="${esc(h.name || "")}">` +
      badge +
      `<label class="hx-confirm" title="Press Enter through its startup prompts"><input type="checkbox" ${h.autoconfirm ? "checked" : ""}>auto-ok</label>` +
      (h.source ? "" : `<button class="rule-del" type="button" title="Remove harness">&times;</button>`) +
      `</div>` +
      `<div class="entry-line">` +
      `<input class="setting-input hx-cmd grow" type="text" spellcheck="false" placeholder="command + flags" value="${esc(h.command || "")}">` +
      `</div>` +
      `<div class="entry-line hx-kinds" title="Which llm account kinds this harness can use; none checked = its default">` +
      `<span class="hx-kinds-label">accounts:</span>` +
      ["anthropic", "openai", "claude", "codex"].map((k) =>
        `<label class="hx-confirm"><input type="checkbox" data-kind="${k}"${(h.accountKinds || []).includes(k) ? " checked" : ""}>${k}</label>`).join("") +
      `</div>`;
    for (const el of row.querySelectorAll("input")) {
      el.addEventListener("change", save);
      el.addEventListener("keydown", (e) => { if (e.key === "Enter") el.blur(); });
    }
    const del = row.querySelector(".rule-del");
    if (del) del.onclick = () => { row.remove(); save(); };
    return row;
  }

  function rowValue(row) {
    const kinds = [...row.querySelectorAll(".hx-kinds input:checked")]
      .map((el) => el.dataset.kind);
    const v = {
      icon: row.querySelector(".hx-icon").value.trim(),
      name: row.querySelector(".hx-name").value.trim(),
      command: row.querySelector(".hx-cmd").value.trim(),
      autoconfirm: row.querySelector(".hx-confirm input").checked,
    };
    if (kinds.length) v.accountKinds = kinds;
    return v;
  }

  function collect() {
    const out = [];
    for (const row of box.querySelectorAll(".entry-card")) {
      const v = rowValue(row);
      if (!v.name && !v.command) continue; // blank editor row
      const untouchedDefault = row.dataset.source &&
        JSON.stringify({ icon: v.icon, name: v.name, command: v.command, autoconfirm: v.autoconfirm, kinds: (v.accountKinds || []).join(", ") }) === row.dataset.orig;
      if (untouchedDefault) continue; // stays live-resolved, not frozen
      out.push(v);
    }
    return out;
  }

  async function put(body) {
    const r = await fetch("/v1/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  }

  const rules = wireHarnessRules(put);

  async function load() {
    try {
      const cfg = await (await fetch("/v1/settings")).json();
      box.innerHTML = "";
      for (const h of cfg.harnesses || []) box.appendChild(harnessRow(h));
      rules.render((cfg.harnesses || []).map((h) => h.name), cfg.harnessRules || []);
      statusEl.textContent = "Installed harnesses appear on their own; edit a row to override it.";
    } catch (_) {
      statusEl.textContent = "Couldn't load harnesses.";
    }
  }

  async function save() {
    try {
      const cfg = await put({ harnesses: collect() });
      // Renames/deletes change what a rule can point at — rebuild the selects.
      rules.render((cfg.harnesses || []).map((h) => h.name), rules.collect());
      statusEl.textContent = "Saved.";
    } catch (e) {
      statusEl.textContent = "Not saved: " + e.message;
    }
  }

  $("open-settings").addEventListener("click", load);
  addBtn.addEventListener("click", () => {
    box.appendChild(harnessRow({}));
    box.lastChild.querySelector(".hx-name").focus();
  });
}
wireHarnessSettings();

// --- settings tabs: one page at a time instead of one long scroll. ---
function wireSettingsTabs() {
  const tabs = [...document.querySelectorAll("#settings-tabs .settings-tab")];
  if (!tabs.length) return;
  for (const tab of tabs) {
    tab.onclick = () => {
      for (const t of tabs) t.classList.toggle("active", t === tab);
      for (const page of document.querySelectorAll(".settings-page")) {
        page.classList.toggle("hidden", page.dataset.page !== tab.dataset.page);
      }
    };
  }
}
wireSettingsTabs();

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
fetchHosts().then(fetchWorkspaces).then(bootDeepLink); // hosts first so deep-link attach dials direct
connectFirehose();
setInterval(fetchWorkspaces, 5000); // reflect status/pane-count changes
