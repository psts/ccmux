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
  tabDrag: null,     // pane id of the tab being dragged along the strip, else null
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
        (q.agent || "") !== (p.agent || "") || (q.agentVersion || "") !== (p.agentVersion || "")) {
      p.atShell = q.atShell;
      p.dormant = q.dormant;
      p.devServer = q.devServer;
      p.harness = q.harness;
      p.agent = q.agent;
      p.agentVersion = q.agentVersion;
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
        `<button class="grp-msgs" title="Peer messages in ${esc(group)}">💬</button>` +
        (win ? `<button class="grp-agents" title="Agents in ${esc(group)}">⚙</button>` : "");
      h.querySelector(".grp-msgs").onclick = (e) => {
        e.stopPropagation();
        window.ccmuxPeers.open(group);
      };
      if (win) {
        h.querySelector(".grp-close").onclick = (e) => {
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
// alphabetically, the ungrouped bucket ("") last; inside a bucket, repos by
// name then agents by name.
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

function wsRow(ws) {
  const active = ws.id === state.wsId;
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
  li.className = "ws" + (active ? " active" : "") + (att ? " att-" + att : "") + (cold ? " cold" : "");
  li.innerHTML =
    `<div class="ws-row">` +
    (agent ? `<span class="exp none"></span>` : `<span class="exp${open ? " open" : " closed"}"></span>`) +
    `<span class="dot ${esc(ws.status)}"></span>` +
    `<span class="name">${esc(ws.name || ws.repoPath)}</span>` +
    wsBadges(ws, agent, cold, running, label) +
    `<button class="more" title="Session menu">⋯</button>` +
    `</div>` +
    (open && !agent ? gitDetail(ws) : "");
  // A cold session has nothing to attach to — clicking revives it in place.
  li.onclick = cold ? () => reviveWorkspace(ws.id) : () => attach(ws.id, null);
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
  sep();
  if (ws.status === "cold") {
    add("Revive", () => reviveWorkspace(ws.id));
  } else {
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
      add(`${h.listening ? "●" : "○"} ${h.name} : ${h.port}`, () => window.open(h.url, "_blank"));
    }
    sep();
    add("Close Session", () => closeSession(ws.id));
  }
  add("Remove Session…", () => removeSession(ws), "danger");

  showMenuAt(menu, x, y);
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
  for (const a of list) {
    // No stand-in glyph for an icon-less base (same rule as the session name).
    const title = a.icon ? `${a.icon} ${a.name}` : a.name;
    if (a.state === "running") {
      add(`● ${title} — open`, () => attach(a.workspace, a.pane));
      add(`■ ${title} — sleep`, () => sleepAgent(win.id, a.name));
      continue;
    }
    const mark = a.state === "asleep" ? "○" : "+";
    const verb = a.state === "asleep" ? "wake…" : "add…";
    add(`${mark} ${title} — ${verb}` + (a.drift ? " ↻" : ""), async () => {
      const text = prompt(`Message ${a.name} (empty = just start it):`, "");
      if (text === null) return;
      const p = await wakeAgent(win.id, a.name, text.trim());
      if (p && p.id) attach(p.workspaceId, p.id);
    });
  }
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
// prefills from the daemon's repo-config detection, like the Mac sheet. The
// port field is blank = "auto": the daemon allocates one from its reserved
// range on save. A suggestion's detected port becomes the row's targetPort
// (kept on the row's dataset and round-tripped — dropping it would erase it
// server-side on every save). Save PUTs and closes only on success; daemon
// validation errors stay inline. ---
let hostnamesWsId = null;

async function openHostnamesModal(ws) {
  hostnamesWsId = ws.id;
  $("hostnames-title").textContent = `Hostnames — ${ws.name}`;
  $("hostnames-error").classList.add("hidden");
  const rows = $("hostnames-rows");
  rows.innerHTML = "";
  let mappings = (ws.hostnames || []).map((h) => ({ name: h.name, port: h.port, targetPort: h.targetPort }));
  let cmd = ws.devCommand || "";
  $("hostnames-cmd-hint").classList.add("hidden");
  try {
    const s = await (await fetch(`/v1/workspaces/${ws.id}/port-suggestions`)).json();
    // autoPort=false: a multi-app repo ccmux cannot steer (e.g. a pnpm
    // monorepo starting two servers) — the detected port must BE the
    // routing port, so prefill it instead of "auto".
    if (!mappings.length) {
      mappings = (s.suggestions || []).map((x) => ({ name: x.name, port: s.autoPort ? "" : x.port, targetPort: x.port }));
    }
    if (!cmd && s.devCommand) $("hostnames-cmd").placeholder = `dev command — detected: ${s.devCommand}`;
    // A stored override whose repo-detected counterpart moved on gets a
    // one-click way back to detection ("" on the daemon = auto-detect).
    if (cmd && s.detectedCommand && s.detectedCommand !== cmd) {
      $("hostnames-cmd-detected").textContent = `repo now detects: ${s.detectedCommand}`;
      $("hostnames-cmd-use").onclick = () => {
        $("hostnames-cmd").value = "";
        $("hostnames-cmd").placeholder = `dev command — detected: ${s.detectedCommand}`;
        $("hostnames-cmd-hint").classList.add("hidden");
      };
      $("hostnames-cmd-hint").classList.remove("hidden");
    }
  } catch (_) { /* suggestions are best-effort */ }
  if (!mappings.length) mappings = [{ name: "", port: "" }];
  for (const m of mappings) rows.appendChild(hostnameRow(m.name, m.port, m.targetPort));
  $("hostnames-cmd").value = cmd;
  $("hostnames-modal").classList.remove("hidden");
}

function hostnameRow(name, port, targetPort) {
  const li = document.createElement("li");
  li.dataset.targetPort = targetPort || "";
  const n = document.createElement("input");
  n.className = "setting-input hn-name";
  n.placeholder = "name";
  n.spellcheck = false;
  n.value = name || "";
  const p = document.createElement("input");
  p.className = "setting-input hn-port";
  p.placeholder = "auto";
  p.title = "Port — leave blank and ccmux assigns one";
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
    // Same rule as the Mac sheet: a typo must not silently become "auto"
    // (port 0 now means "the daemon allocates one").
    let port = 0;
    if (portText) {
      port = /^\d+$/.test(portText) ? parseInt(portText, 10) : 0;
      if (port < 1 || port > 65535) {
        err.textContent = `${name || "row"}: port must be 1–65535, or blank for auto`;
        err.classList.remove("hidden");
        return;
      }
    }
    hostnames.push({ name, port, targetPort: parseInt(li.dataset.targetPort, 10) || 0 });
  }
  const body = { hostnames, devCommand: $("hostnames-cmd").value.trim() };
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
function renderTabs() {
  if (state.tabDrag) return; // rebuilding the strip mid-drag would orphan the drag
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
    b.title = p.agent ? `agent ${p.agent}` + (p.atShell || p.dormant ? " — asleep" : "")
      : p.dormant ? "Claude exited — shell only" : "";
    b.textContent = (p.agent ? "⚙ " : "") + (p.title || `pane ${i + 1}`);
    // A drop ends with a click on the same button; that click must not switch panes.
    b.onclick = () => { if (!state.tabDrag) attach(state.wsId, p.id); };
    b.oncontextmenu = (e) => { e.preventDefault(); if (!state.tabDrag) openPaneLLMMenu(p.id, e.clientX, e.clientY); };
    wireTabDrag(b, p.id);
    tabs.appendChild(b);
  });
}

// Right-click on a pane tab: pick which LLM account answers THIS pane. The
// choice applies to the pane's next request — no restart. "Use the harness
// order" clears the override so the pane falls back to what its harness
// declares (and, with no harness, to the default account).
//
// The header shows the order the proxy would ACTUALLY try, which is not the
// configured order: an account at its limit has already sunk to the back of
// it, so a pane answering from the second account says so here.
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
    // The chat rides opencode's server; an agent on another harness (claude)
    // keeps its terminal, which is where that harness talks.
    if (p.harness === "opencode") window.ccmuxAgentChat.show(p, state.workspaces.find((w) => w.id === state.wsId));
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

  // The sidecar footer of a meridian account: the daemon runs one Meridian
  // process per such account (its key is the Claude setup token that process
  // authenticates with), so "is it running" is the status that matters.
  function sidecarLine(a, sc) {
    if (a.kind !== "meridian") return "";
    if (!sc) return "⚙ sidecar not supervised by this daemon";
    if (sc.running) return `⚙ sidecar running (pid ${sc.pid}${sc.restarts ? `, ${sc.restarts} restarts` : ""})`;
    return "⚙ sidecar stopped" + (sc.lastError ? ": " + sc.lastError : "");
  }

  const accountKinds = ["anthropic", "openai", "claude", "codex", "meridian"];

  function keyHintFor(a) {
    if (a.apiKeySet) return "set — empty keeps it";
    if (a.kind === "claude") return "paste `claude setup-token` output";
    if (a.kind === "meridian") return "paste `claude setup-token` output (starts the sidecar)";
    return "empty = your own login";
  }

  // The list is a SUMMARY: what each account is, how it is doing, and where
  // it sits in the failover order. Everything you set rather than read — URL,
  // key, aliases, the model map — lives in the modal, because this tab is
  // read far more often than it is edited and the order arrows are the thing
  // you reach for.
  //
  // `accounts` is the record the rows render from, not the DOM. Reading edits
  // back out of input fields is what made every keystroke PUT the whole list.
  let accounts = [];
  let statuses = {};
  let sidecars = {};

  function accountRow(a, i) {
    const row = document.createElement("div");
    row.className = "entry-card";
    const status = [statusLine(statuses[a.name]), sidecarLine(a, sidecars[a.name])].filter(Boolean).join(" · ");
    row.innerHTML =
      `<div class="entry-line">` +
      `<span class="llm-pos">${i + 1}</span>` +
      `<span class="llm-acct-name grow">${esc(a.name || "(unnamed)")}</span>` +
      `<span class="llm-kind">${esc(a.kind || "anthropic")}</span>` +
      `<button class="ord-up" type="button" title="Try this account earlier">\u25b2</button>` +
      `<button class="ord-dn" type="button" title="Try this account later">\u25bc</button>` +
      `<button class="rule-add llm-edit" type="button">Edit</button>` +
      `<button class="rule-del" type="button" title="Remove account">&times;</button>` +
      `</div>` +
      (status ? `<div class="entry-line llm-acct-status">${esc(status)}</div>` : "");
    row.querySelector(".llm-edit").onclick = () => openAccount(i);
    row.querySelector(".ord-up").onclick = () => moveAccount(i, -1);
    row.querySelector(".ord-dn").onclick = () => moveAccount(i, 1);
    row.querySelector(".rule-del").onclick = () => {
      if (!confirm(`Remove account "${a.name}"? Its stored key goes with it.`)) return;
      queueSave((list) => list.filter((x) => x.name !== a.name));
    };
    return row;
  }

  // Row actions run through queueSave and address accounts by NAME rather
  // than by the index the row was drawn with. Both matter together: a save is
  // a round trip, and a second click during one would otherwise build its
  // change from the pre-save list — deleting two accounts in quick succession
  // re-added the first, keyless, because the second request still carried it
  // and the daemon inherits a key only from what it currently stores.
  let saving = Promise.resolve();

  function queueSave(mutate, opts) {
    const next = saving.catch(() => {}).then(() => saveAccounts(mutate(accounts.slice()), opts));
    saving = next.catch(() => {}); // a refusal must not wedge the queue
    return next;
  }

  // The stored order IS the failover order, so moving a row is a real setting.
  function moveAccount(i, delta) {
    const name = (accounts[i] || {}).name;
    queueSave((list) => {
      const at = list.findIndex((x) => x.name === name);
      const to = at + delta;
      if (at < 0 || to < 0 || to >= list.length) return list;
      const next = list.slice();
      [next[at], next[to]] = [next[to], next[at]];
      return next;
    });
  }

  function renderAccounts() {
    box.innerHTML = "";
    accounts.forEach((a, i) => box.appendChild(accountRow(a, i)));
  }

  // --- the one-account editor ---
  const modal = () => $("llm-modal");
  const mq = (sel) => modal().querySelector(sel);
  let editing = -1;

  function buildModal() {
    const m = modal();
    if (m.dataset.built) return;
    m.dataset.built = "1";
    m.querySelector(".llm-modal-body").innerHTML =
      `<div class="entry-line">` +
      `<input class="setting-input lm-name grow" type="text" spellcheck="false" placeholder="name">` +
      `<select class="setting-input lm-kind">` +
      accountKinds.map((k) => `<option value="${k}">${k}</option>`).join("") +
      `</select></div>` +
      `<div class="entry-line"><input class="setting-input lm-url grow" type="text" spellcheck="false" placeholder="base URL"></div>` +
      `<div class="entry-line"><input class="setting-input lm-key grow" type="password" autocomplete="off" placeholder="token / api key"></div>` +
      `<div class="entry-line">` +
      `<input class="setting-input lm-aliases grow" type="text" spellcheck="false" placeholder="aliases: claude-haiku-*=qwen3-4b-32k">` +
      `<select class="setting-input lm-model-pick" title="List the upstream's models; picking one maps every claude-* request to it">` +
      `<option value="">map claude → …</option></select>` +
      `</div>` +
      `<p class="hint lm-status"></p>`;
    $("llm-modal-close").onclick = closeModal;
    m.onclick = (e) => { if (e.target === m) closeModal(); };
    $("llm-modal-save").onclick = saveModal;
    // Picking a model rewrites the alias field: claude-* rules replaced,
    // custom rules kept.
    mq(".lm-model-pick").addEventListener("change", (e) => {
      const model = e.target.value;
      if (!model) return;
      const field = mq(".lm-aliases");
      const kept = parseAliases(field.value).filter((x) => !x.from.startsWith("claude-"));
      field.value = kept.concat([{ from: "claude-*", to: model }])
        .map((x) => `${x.from}=${x.to}`).join(", ");
    });
    mq(".lm-kind").addEventListener("change", () => {
      mq(".lm-key").placeholder = "token / api key: " + keyHintFor({ kind: mq(".lm-kind").value });
    });
  }

  // Bumped on every open. fillModelPick captures it and drops a response
  // that arrives after the sheet moved on: the modal is ONE element reused
  // for every account, so a slow upstream would otherwise append its models
  // to whichever account you opened next, and picking one would alias that
  // account to a model its own upstream has never heard of.
  let openGeneration = 0;

  function openAccount(i) {
    buildModal();
    editing = i;
    openGeneration++;
    const a = accounts[i] || {};
    mq(".lm-name").value = a.name || "";
    mq(".lm-kind").value = a.kind || "anthropic";
    mq(".lm-url").value = a.baseURL || "";
    mq(".lm-url").placeholder = a.kind === "meridian"
      ? "base URL (empty = http://127.0.0.1:3456)" : "base URL, e.g. http://localhost:11434";
    mq(".lm-key").value = "";
    mq(".lm-key").placeholder = "token / api key: " + keyHintFor(a);
    mq(".lm-aliases").value = (a.modelAliases || []).map((x) => `${x.from}=${x.to}`).join(", ");
    mq(".lm-status").textContent = [statusLine(statuses[a.name]), sidecarLine(a, sidecars[a.name])].filter(Boolean).join(" · ");
    $("llm-modal-title").textContent = a.name ? "Account · " + a.name : "New account";
    $("llm-modal-state").textContent = "";
    modal().classList.remove("hidden");
    // One upstream, asked when you open it — the list used to ask every
    // account's upstream on every settings open.
    fillModelPick(a, openGeneration);
    mq(".lm-name").focus();
  }

  function closeModal() { modal().classList.add("hidden"); editing = -1; }

  // Fill the model picker from this account's upstream. An upstream that
  // doesn't answer says why rather than leaving an empty picker that reads
  // as a broken feature.
  async function fillModelPick(a, generation) {
    const sel = mq(".lm-model-pick");
    const current = ((a.modelAliases || []).find((x) => x.from === "claude-*") || {}).to || "";
    sel.innerHTML = `<option value="">map claude → …</option>` +
      (current ? `<option value="${esc(current)}" selected>${esc(current)}</option>` : "");
    if (!a.name) return; // unsaved: there is no upstream to ask yet
    try {
      const r = await fetch(`/v1/llm/accounts/${encodeURIComponent(a.name)}/models`);
      if (generation !== openGeneration) return; // the sheet moved on
      if (!r.ok) {
        const msg = (await r.json().catch(() => ({}))).error || `HTTP ${r.status}`;
        if (generation !== openGeneration) return; // that await let the sheet move on
        sel.title = "couldn't list models: " + msg;
        sel.options[0].textContent = "models unavailable";
        return;
      }
      const list = (await r.json()).models || [];
      if (generation !== openGeneration) return;
      for (const m of list) {
        if (m === current) continue;
        const o = document.createElement("option");
        o.value = m; o.textContent = m;
        sel.appendChild(o);
      }
    } catch (_) {
      // Guarded like the success paths: a slow REJECTION would otherwise
      // stamp "models unavailable" over the next account's own list.
      if (generation !== openGeneration) return;
      sel.options[0].textContent = "models unavailable";
    }
  }

  async function saveModal() {
    const name = mq(".lm-name").value.trim();
    if (!name) { $("llm-modal-state").textContent = "An account needs a name."; return; }
    const next = {
      name,
      kind: mq(".lm-kind").value,
      baseURL: mq(".lm-url").value.trim(),
      modelAliases: parseAliases(mq(".lm-aliases").value),
    };
    // Write-only key: an empty box KEEPS the stored one, which is what lets
    // this form round-trip a redacted account without wiping its secret.
    const key = mq(".lm-key").value;
    if (key) next.apiKey = key;
    else if (accounts[editing]) next.apiKeySet = accounts[editing].apiKeySet;
    const wasNamed = (accounts[editing] || {}).name;
    try {
      await queueSave((list) => {
        const at = wasNamed ? list.findIndex((x) => x.name === wasNamed) : -1;
        const candidate = list.slice();
        if (at >= 0) candidate[at] = next;
        else candidate.push(next);
        return candidate;
      }, { rethrow: true });
      closeModal();
    } catch (e) {
      // `accounts` is untouched, so the rows still show what the daemon
      // holds and the sheet stays open on the edit that was refused.
      $("llm-modal-state").textContent = "Not saved: " + e.message;
    }
  }

  function parseAliases(text) {
    return text.split(",").map((part) => {
      const [from, ...rest] = part.split("=");
      return { from: (from || "").trim(), to: rest.join("=").trim() };
    }).filter((x) => x.from && x.to);
  }

  function renderRoute(list, route) {
    routeSel.innerHTML = "";
    const direct = document.createElement("option");
    direct.value = ""; direct.textContent = "Anthropic (direct, your Claude login)";
    routeSel.appendChild(direct);
    for (const a of list) {
      if (!a.name) continue;
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
      statuses = {};
      for (const st of cfg.llmAccountStatus || []) statuses[st.name] = st;
      sidecars = cfg.llmSidecars || {};
      accounts = (cfg.llmAccounts || []).map((a) => ({ ...a }));
      renderAccounts();
      renderRoute(accounts, cfg.llmRoute);
      statusEl.textContent = "Order decides who answers first; a limited account drops to the back on its own. Applies to every pane's next request — no restarts.";
    } catch (_) {
      statusEl.textContent = "Couldn't load LLM settings.";
    }
  }

  // saveAccounts takes the list to PUT rather than reading the module one,
  // and `accounts` only advances when the daemon accepted it. Mutating first
  // and saving second is what let a REFUSED edit sit in memory until the next
  // unrelated row click PUT it — silently, and without the key it never
  // carried, so the stored credential went with it.
  async function saveAccounts(candidate, opts) {
    try {
      const cfg = await put({ llmAccounts: candidate.map(({ apiKeySet, ...rest }) => rest) });
      // Re-seed from the daemon's echo: it normalizes URLs and reports which
      // accounts hold a key, neither of which this side should guess at.
      accounts = (cfg.llmAccounts || []).map((a) => ({ ...a }));
      renderAccounts();
      renderRoute(accounts, cfg.llmRoute);
      statusEl.textContent = "Saved.";
    } catch (e) {
      statusEl.textContent = "Not saved: " + e.message;
      if (opts && opts.rethrow) throw e;
      load(); // the list now lies — reload truth
    }
  }

  $("open-settings").addEventListener("click", load);
  routeSel.addEventListener("change", async () => {
    try {
      await put({ llmRoute: routeSel.value });
      routeStateEl.textContent = routeSel.value
        ? `Panes with no harness of their own use ${routeSel.value}.`
        : "Panes with no harness of their own go direct to Anthropic.";
    } catch (e) {
      routeStateEl.textContent = "Not saved: " + e.message;
      load(); // the picker now lies — reload truth
    }
  });
  addBtn.addEventListener("click", () => openAccount(accounts.length));
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
  // The accounts a harness could use, newest load wins. Needed here because
  // the per-harness order names accounts, and the list it offers must match
  // what the Accounts tab holds.
  let accounts = [];

  // kindAllowed, mirrored from the daemon (llmproxy.KindAllowed). Empty means
  // any kind except the two a harness has to ask for by name.
  function kindAllowed(kinds, kind) {
    if (!kinds || !kinds.length) return kind !== "codex" && kind !== "meridian";
    return kinds.includes(kind);
  }

  // A codex account speaks OpenAI's Responses API; every other kind speaks
  // Anthropic's Messages API. A harness that checks both gets a warning, not
  // a refusal: an unknown plugin may genuinely need the override, and the
  // proxy holds each pane's failover order to ONE dialect regardless.
  function dialectWarning(kinds) {
    const codex = kinds.includes("codex");
    const other = kinds.some((k) => k !== "codex");
    return codex && other
      ? "codex speaks a different API than the other kinds — a pane will use whichever one answers first and ignore the rest"
      : "";
  }

  // The accounts this row may use, in the order it would try them: the ones
  // it named first, then the rest as configured. A named account that no
  // longer exists is dropped here the same way the proxy drops it.
  function orderedFor(kinds, order) {
    const allowed = accounts.filter((a) => kindAllowed(kinds, a.kind));
    const named = (order || [])
      .map((n) => allowed.find((a) => a.name === n))
      .filter(Boolean);
    return named.concat(allowed.filter((a) => !named.includes(a)));
  }

  function harnessRow(h) {
    const row = document.createElement("div");
    row.className = "entry-card";
    const kindsText = (h.accountKinds || []).join(", ");
    const custom = !!(h.accountOrder || []).length;
    // Radio groups are keyed by name, so each row needs its own or checking
    // one harness's radio would clear every other row's.
    const radioName = `hx-order-${Math.random().toString(36).slice(2)}`;
    row.dataset.orig = JSON.stringify({ icon: h.icon || "", name: h.name || "", command: h.command || "", autoconfirm: !!h.autoconfirm, kinds: kindsText, order: (h.accountOrder || []).join(", ") });
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
      ["anthropic", "openai", "claude", "codex", "meridian"].map((k) =>
        `<label class="hx-confirm"><input type="checkbox" data-kind="${k}"${(h.accountKinds || []).includes(k) ? " checked" : ""}>${k}</label>`).join("") +
      `</div>` +
      `<div class="entry-line hx-dialect hint"></div>` +
      `<div class="entry-line hx-order-pick">` +
      `<span class="hx-kinds-label">order:</span>` +
      `<label class="hx-confirm"><input type="radio" name="${esc(radioName)}" value="global"${custom ? "" : " checked"}>follow the account order</label>` +
      `<label class="hx-confirm"><input type="radio" name="${esc(radioName)}" value="custom"${custom ? " checked" : ""}>custom for this harness</label>` +
      `</div>` +
      `<div class="entry-line hx-order"></div>`;
    // Checking a kind changes which accounts the order may hold, and the
    // radio decides whether the list shows at all. Both must redraw BEFORE
    // the save: rowValue reads the order back off the DOM, so saving first
    // would send the list as it was — for the radio that means turning
    // "custom" on and persisting no order at all, and the choice silently
    // reverting on the next load.
    for (const el of row.querySelectorAll(".hx-kinds input, .hx-order-pick input")) {
      el.addEventListener("change", () => renderOrder(row));
    }
    for (const el of row.querySelectorAll("input")) {
      el.addEventListener("change", save);
      el.addEventListener("keydown", (e) => { if (e.key === "Enter") el.blur(); });
    }
    const del = row.querySelector(".rule-del");
    if (del) del.onclick = () => { row.remove(); save(); };
    row.dataset.order = JSON.stringify(h.accountOrder || []);
    renderOrder(row);
    return row;
  }

  // renderOrder redraws one row's account list from its checked kinds and
  // its stored order. Held in row.dataset rather than read back off the DOM
  // so a kind the user unchecks and rechecks does not lose its position.
  function renderOrder(row) {
    const listEl = row.querySelector(".hx-order");
    const warnEl = row.querySelector(".hx-dialect");
    const kinds = [...row.querySelectorAll(".hx-kinds input:checked")].map((el) => el.dataset.kind);
    warnEl.textContent = dialectWarning(kinds);
    const custom = row.querySelector(".hx-order-pick input[value=custom]").checked;
    if (!custom) {
      listEl.innerHTML = "";
      return;
    }
    const stored = JSON.parse(row.dataset.order || "[]");
    const ordered = orderedFor(kinds, stored);
    // Seed the stored order from what the row resolves to today, so turning
    // "custom" on freezes the current list (which is the whole point of the
    // choice) instead of persisting an empty override that reads as "follow
    // the account order" on the next load.
    //
    // Names already stored are KEPT at their positions even when the current
    // kinds filter them out, so unchecking a kind parks its accounts rather
    // than erasing them: recheck it and the order the user set is still
    // there. The daemon skips a name its kinds do not allow anyway.
    const next = stored.slice();
    for (const a of ordered) if (!next.includes(a.name)) next.push(a.name);
    row.dataset.order = JSON.stringify(next);
    if (!ordered.length) {
      // The radio stays on custom and says why. An empty custom order still
      // persists as no override, because it resolves identically to
      // following the account order — same rule as the Mac lens.
      listEl.innerHTML = `<span class="hint">no account matches the kinds above</span>`;
      return;
    }
    listEl.innerHTML = ordered.map((a, i) =>
      `<span class="hx-order-item" data-name="${esc(a.name)}">` +
      `${i + 1}. ${esc(a.name)} <span class="llm-kind">${esc(a.kind)}</span>` +
      `<button class="ord-up" type="button" title="Try this account earlier">\u25b2</button>` +
      `<button class="ord-dn" type="button" title="Try this account later">\u25bc</button>` +
      `</span>`).join("");
    const names = ordered.map((a) => a.name);
    listEl.querySelectorAll(".hx-order-item").forEach((item, i) => {
      item.querySelector(".ord-up").onclick = () => moveInOrder(row, names, i, -1);
      item.querySelector(".ord-dn").onclick = () => moveInOrder(row, names, i, 1);
    });
  }

  // Swaps two VISIBLE rows inside the row's stored order. It works on the
  // stored array rather than replacing it with the painted list, because the
  // painted list is kind-filtered: rebuilding from it would drop the accounts
  // an unchecked kind has parked, undoing the whole point of parking them.
  function moveInOrder(row, names, i, delta) {
    const j = i + delta;
    if (j < 0 || j >= names.length) return;
    const stored = JSON.parse(row.dataset.order || "[]");
    const a = stored.indexOf(names[i]), b = stored.indexOf(names[j]);
    if (a < 0 || b < 0) return; // renderOrder seeds every visible name first
    [stored[a], stored[b]] = [stored[b], stored[a]];
    row.dataset.order = JSON.stringify(stored);
    renderOrder(row);
    save();
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
    // Only a custom order is sent: "follow the account order" is the absence
    // of the field, which is what makes the global order keep reaching this
    // harness as accounts are added and moved.
    if (row.querySelector(".hx-order-pick input[value=custom]").checked) {
      // From the row's stored order, not from the painted list: the dataset
      // is the single record renderOrder and moveInOrder both write, so a
      // save cannot depend on whether the DOM has been redrawn yet.
      const order = JSON.parse(row.dataset.order || "[]");
      if (order.length) v.accountOrder = order;
    }
    return v;
  }

  function collect() {
    const out = [];
    for (const row of box.querySelectorAll(".entry-card")) {
      const v = rowValue(row);
      if (!v.name && !v.command) continue; // blank editor row
      const untouchedDefault = row.dataset.source &&
        JSON.stringify({ icon: v.icon, name: v.name, command: v.command, autoconfirm: v.autoconfirm, kinds: (v.accountKinds || []).join(", "), order: (v.accountOrder || []).join(", ") }) === row.dataset.orig;
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
      accounts = cfg.llmAccounts || []; // before the rows: they render from it
      box.innerHTML = "";
      for (const h of cfg.harnesses || []) box.appendChild(harnessRow(h));
      rules.render((cfg.harnesses || []).map((h) => h.name), cfg.harnessRules || []);
      statusEl.textContent = "Installed harnesses appear on their own; edit a row to override it. A harness tries its accounts top-down and falls through when one hits its limit.";
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

// --- Agents tab: base agents are folders the daemon owns, listed here; New
// and Edit open the editor (agentmodal.js), which does the PUT. Delete is
// explicit and confirmed: the folder holds skills a human wrote. ---
function wireAgentSettings() {
  const box = $("agent-list"), addBtn = $("agent-add"), statusEl = $("agent-state");
  if (!box) return;
  let cfg = {};

  function agentRow(a) {
    const row = document.createElement("div");
    row.className = "entry-card agent-card";
    row.innerHTML =
      `<div class="entry-line">` +
      `<span class="agent-icon">${esc(a.icon || "")}</span>` +
      `<span class="agent-name">${esc(a.name)}</span>` +
      `<span class="agent-version">v${esc(a.version || "")}</span>` +
      `<span class="agent-harness">${esc(a.harness || "")}</span>` +
      `<span class="grow"></span>` +
      `<button class="rule-add ag-edit" type="button">Edit</button>` +
      `<button class="rule-del" type="button" title="Delete agent and its folder">&times;</button>` +
      `</div>` +
      `<div class="agent-desc">${esc(a.description || "")}</div>`;
    row.querySelector(".ag-edit").onclick = () => window.ccmuxAgentModal.open(a, cfg, load);
    row.querySelector(".rule-del").onclick = () => deleteAgent(a.name);
    return row;
  }

  async function deleteAgent(name) {
    if (!confirm(`Delete agent "${name}" and its folder (skills included)? Project instance folders stay.`)) return;
    try {
      const r = await fetch(`/v1/agents/${encodeURIComponent(name)}`, { method: "DELETE" });
      if (!r.ok && r.status !== 404) throw new Error(await r.text());
    } catch (e) {
      statusEl.textContent = "Not deleted: " + e.message;
      return;
    }
    statusEl.textContent = `Deleted ${name}.`;
    agentCatalog.at = 0;
    load();
  }

  async function load() {
    try {
      const [c, r] = await Promise.all([(await fetch("/v1/settings")).json(), fetch("/v1/agents")]);
      cfg = c;
      box.innerHTML = "";
      if (r.status === 503) { statusEl.textContent = "This daemon has no agents support."; addBtn.disabled = true; return; }
      addBtn.disabled = false;
      if (!r.ok) { statusEl.textContent = "Couldn't load agents: " + (await r.text()); return; } // names the broken folder
      const list = (await r.json()).agents || [];
      for (const a of list) box.appendChild(agentRow(a));
      statusEl.textContent = list.length ? "" : "No agents yet.";
      agentCatalog.at = 0;
    } catch (_) {
      statusEl.textContent = "Couldn't load agents.";
    }
  }

  $("open-settings").addEventListener("click", load);
  addBtn.addEventListener("click", () => window.ccmuxAgentModal.open(null, cfg, load));
}
wireAgentSettings();

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
// Every no-answer path hides the strip, and the Mac lens applies the identical
// rule (Sources/ccmux/Services/DaemonHealthService.swift). That symmetry is the
// point: keeping the last reading on a failed poll meant the strip kept saying
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
