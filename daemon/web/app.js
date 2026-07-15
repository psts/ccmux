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
  paneCols: 0,       // authoritative width of the current pane (from the daemon)
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

// paneColsOf returns a pane's daemon-reported width (0 if unknown).
function paneColsOf(paneId) {
  const p = state.panes.find((x) => x.id === paneId);
  return (p && p.cols) || 0;
}

// --- attach / websocket ---
function attach(wsId, wantPane) {
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

  const proto = location.protocol === "https:" ? "wss" : "ws";
  const q = `workspace=${wsId}&user=${encodeURIComponent(getUser())}&device=web`;
  const conn = new WebSocket(`${proto}://${location.host}/v1/attach?${q}`);
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
$("menu-toggle").onclick = toggleDrawer;
$("drawer-backdrop").onclick = closeDrawer;
$("takeover").onclick = takeOver;
fetchWorkspaces().then(bootDeepLink);
connectFirehose();
setInterval(fetchWorkspaces, 5000); // reflect status/pane-count changes
