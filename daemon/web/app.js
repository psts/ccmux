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
};

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s).replace(/[<>&]/g, (c) => ({ "<": "&lt;", ">": "&gt;", "&": "&amp;" }[c]));

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
    const li = document.createElement("li");
    li.className = "ws" + (ws.id === state.wsId ? " active" : "");
    li.innerHTML =
      `<span class="dot ${esc(ws.status)}"></span>` +
      `<span class="name">${esc(ws.name || ws.repoPath)}</span>` +
      `<span class="count">${(ws.panes || []).length}</span>`;
    li.onclick = () => attach(ws.id, null);
    ul.appendChild(li);
  }
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
  $("empty").style.display = "none";
  ensureTerm();
  state.term.reset();
  renderList();

  const proto = location.protocol === "https:" ? "wss" : "ws";
  const conn = new WebSocket(`${proto}://${location.host}/v1/attach?workspace=${wsId}`);
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
      break;
    case "snapshot":
    case "output":
      if (m.pane === state.paneId && m.data) state.term.write(b64ToBytes(m.data));
      break;
    case "attention":
      setAttention(m.pane, m.state);
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

function setAttention(paneId, stateStr) {
  const p = state.panes.find((x) => x.id === paneId);
  if (p) p.attention = stateStr;
  const btn = document.querySelector(`.tab[data-pane="${paneId}"]`);
  if (btn) btn.className = btn.className.replace(/att-\w+/, "att-" + stateStr);
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

// --- boot ---
$("new-ws").onclick = newWorkspace;
fetchWorkspaces();
setInterval(fetchWorkspaces, 5000); // reflect status/pane-count changes
