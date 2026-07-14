// ccmux web lens — a thin xterm.js client over the daemon's REST + WS API.
// No build step: vanilla JS against the globals Terminal (xterm.js) and
// FitAddon (addon-fit), served embedded from ccmuxd.
"use strict";

const state = {
  workspaces: [],
  wsId: null,
  paneId: null,      // currently rendered pane
  wantPane: null,    // pane to select after the next attach (for tab switches)
  panes: [],
  conn: null,
  term: null,
  fit: null,
  firehose: null,    // global /v1/events WS (sidebar attention, all workspaces)
  attn: {},          // wsId -> { paneId -> attentionState } from the firehose
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
  renderList();
}

function renderList() {
  const ul = $("ws-list");
  ul.innerHTML = "";
  for (const ws of state.workspaces) {
    const active = ws.id === state.wsId;
    // Suppress the flash on the workspace you're already watching (mirrors the
    // native "clear on watch"); other rows flash live from the firehose.
    const att = active ? "" : wsAttention(ws);
    const li = document.createElement("li");
    li.className = "ws" + (active ? " active" : "") + (att ? " att-" + att : "");
    li.innerHTML =
      `<span class="dot ${esc(ws.status)}"></span>` +
      `<span class="name">${esc(ws.name || ws.repoPath)}</span>` +
      `<span class="count">${(ws.panes || []).length}</span>`;
    li.onclick = () => attach(ws.id, null);
    ul.appendChild(li);
  }
}

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
  window.addEventListener("resize", doFit);
  // Focus reporting powers push suppression: while this lens is actively watching
  // the daemon holds back notifications; when the tab is hidden or blurred we
  // clear focus so a "needs input" push comes through.
  document.addEventListener("visibilitychange", reportFocus);
  window.addEventListener("focus", reportFocus);
  window.addEventListener("blur", reportFocus);
}

function reportFocus() {
  const watching = document.visibilityState === "visible" && document.hasFocus();
  send({ t: "focus", pane: watching ? state.paneId : "" });
}

function doFit() {
  if (!state.fit || !state.term) return;
  try { state.fit.fit(); } catch (_) {}
  send({ t: "resize", pane: state.paneId, cols: state.term.cols, rows: state.term.rows });
}

// --- attach / websocket ---
function attach(wsId, wantPane) {
  if (state.conn) { state.conn.close(); state.conn = null; }
  state.wsId = wsId;
  state.wantPane = wantPane;
  delete state.attn[wsId]; // opening it marks its attention seen
  $("empty").style.display = "none";
  ensureTerm();
  state.term.reset();
  renderList();

  const proto = location.protocol === "https:" ? "wss" : "ws";
  const q = `workspace=${wsId}&user=${encodeURIComponent(getUser())}&device=web`;
  const conn = new WebSocket(`${proto}://${location.host}/v1/attach?${q}`);
  conn.onmessage = onMessage;
  conn.onopen = () => doFit();
  state.conn = conn;
}

function onMessage(ev) {
  const m = JSON.parse(ev.data);
  switch (m.t) {
    case "hello":
      state.panes = m.panes || [];
      state.paneId = state.wantPane || (state.panes[0] && state.panes[0].id) || null;
      state.wantPane = null;
      renderTabs();
      doFit();
      reportFocus();
      break;
    case "snapshot":
    case "output":
      if (m.pane === state.paneId && m.data) state.term.write(b64ToBytes(m.data));
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
  } else if (m.t === "workspace-added" || m.t === "workspace-removed" || m.t === "workspace-status") {
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

// --- new workspace ---
async function newWorkspace() {
  const repoPath = prompt("Repo path on the host:", "");
  if (!repoPath) return;
  const startupCommand = prompt("Startup command (optional, e.g. claude):", "") || "";
  const r = await fetch("/v1/workspaces", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name: repoPath.split("/").pop(), repoPath, startupCommand, createdBy: "web" }),
  });
  if (!r.ok) { alert("create failed: " + (await r.text())); return; }
  const ws = await r.json();
  await fetchWorkspaces();
  attach(ws.id, null);
}

// Opened from a notification tap (/?ws=<id>): attach straight to that workspace.
function bootDeepLink() {
  const ws = new URLSearchParams(location.search).get("ws");
  if (ws) attach(ws, null);
}

// --- boot ---
window.ccmux = { attach, getUser }; // push.js deep-links + shares the presence name
$("new-ws").onclick = newWorkspace;
fetchWorkspaces().then(bootDeepLink);
connectFirehose();
setInterval(fetchWorkspaces, 5000); // reflect status/pane-count changes
