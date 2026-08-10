// Read-only peer-messages viewer for a window group: history + live stream
// from the daemon's built-in peers bus (/v1/peers/*). Opened from a group
// header's chat button; strictly a viewer — sending stays in the sessions.
"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const esc = (s) => String(s).replace(/[<>&]/g, (c) => ({ "<": "&lt;", ">": "&gt;", "&": "&amp;" }[c]));

  let group = null; // open group, null when the modal is closed
  let sock = null;
  // Which bus to read and the read-only credential for it. On a hub (or a lone
  // node) bus is "" and the local routes answer; on a member host the sessions
  // have federated onto the hub, so the reads go through this daemon's relay —
  // reading the local registry there shows an empty panel while every session it
  // asks about is somewhere else.
  let viewer = { bus: "", token: "" };
  // Set when the daemon would not tell us which bus to read. The local routes
  // still answer, but on a member host they answer for a registry nobody is in —
  // so an empty result after this is not evidence of silence and must not be
  // drawn as "no messages".
  let busUnknown = false;

  async function open(g) {
    group = g;
    $("peers-title").textContent = "Messages — " + g.toUpperCase();
    $("peers-modal").classList.remove("hidden");
    $("peers-status").textContent = "Loading…";
    $("peers-status").classList.remove("hidden");
    $("peers-list").innerHTML = "";
    $("peers-msgs").innerHTML = "";
    // Asked on every open: a hub can appear or move while this page is loaded.
    viewer = { bus: "", token: "" };
    busUnknown = false;
    try {
      const r = await fetch("/v1/peers/viewer");
      if (r.ok) viewer = await r.json();
      else busUnknown = true;
    } catch (_) {
      busUnknown = true;
    }
    if (group !== g) return; // closed or reopened while we asked
    refresh();
    connect();
  }

  const auth = () => (viewer.token ? { Authorization: "Bearer " + viewer.token } : {});

  // A refusal is not data. Decoding one as JSON yields an object with no
  // messages in it, which renders as silence — the exact failure this viewer was
  // rebuilt to stop showing.
  function refusal(status) {
    if (status === 503) return "ccmuxd can't reach the hub right now — the sessions live there, so this list would be wrong.";
    if (status === 401 || status === 403) return "ccmuxd refused this page's read of the peers bus.";
    return "The peers bus answered HTTP " + status + ".";
  }

  async function readJSON(path) {
    const r = await fetch(viewer.bus + path, { headers: auth() });
    if (!r.ok) throw new Error(refusal(r.status));
    return r.json();
  }

  function close() {
    group = null;
    if (sock) { sock.close(); sock = null; }
    $("peers-modal").classList.add("hidden");
  }

  async function refresh() {
    if (!group) return;
    const q = "group=" + encodeURIComponent(group);
    let msgs = [], peers = [];
    try {
      [msgs, peers] = await Promise.all([
        readJSON("/v1/peers/messages?" + q + "&limit=200"),
        readJSON("/v1/peers?" + q),
      ]);
    } catch (e) {
      $("peers-status").textContent = e.message;
      $("peers-status").classList.remove("hidden");
      return;
    }
    renderPeers(peers || []);
    const box = $("peers-msgs");
    box.innerHTML = "";
    for (const m of msgs || []) box.appendChild(msgRow(m));
    $("peers-status").classList.toggle("hidden", (msgs || []).length > 0);
    if ((msgs || []).length === 0) {
      $("peers-status").textContent = busUnknown
        ? "Couldn't confirm which bus to read, so this may not be the whole picture."
        : "No messages yet.";
    }
    box.scrollTop = box.scrollHeight;
  }

  function connect() {
    if (sock) { sock.close(); sock = null; }
    if (!group) return;
    const proto = location.protocol === "https:" ? "wss" : "ws";
    // The WebSocket constructor takes no headers, so the credential rides the
    // query string on this hop. The relay strips it before the request crosses
    // to the hub.
    // Only through the relay: the local route ignores it, and a token in a URL
    // is one more place it can be logged.
    const tok = viewer.bus && viewer.token ? "&viewer_token=" + encodeURIComponent(viewer.token) : "";
    const ws = new WebSocket(
      `${proto}://${location.host}${viewer.bus}/v1/peers/ws?mode=listen&group=${encodeURIComponent(group)}${tok}`);
    ws.onmessage = (ev) => {
      let m;
      try { m = JSON.parse(ev.data); } catch (_) { return; }
      if (m.type !== "message") return;
      $("peers-status").classList.add("hidden");
      const box = $("peers-msgs");
      const follow = box.scrollTop + box.clientHeight >= box.scrollHeight - 30;
      box.appendChild(msgRow(m));
      if (follow) box.scrollTop = box.scrollHeight;
    };
    ws.onclose = () => {
      if (ws !== sock) return; // superseded or modal closed
      sock = null;
      setTimeout(() => { if (group) connect(); }, 2000);
    };
    sock = ws;
  }

  function renderPeers(peers) {
    const ul = $("peers-list");
    ul.innerHTML = "";
    for (const p of peers) {
      const li = document.createElement("li");
      li.className = "peer-chip" + (p.connected ? "" : " off");
      li.innerHTML = `<span class="pdot"></span><span class="pname">${esc(p.name || p.id)}</span>` +
        (p.summary ? `<span class="psum">${esc(p.summary)}</span>` : "");
      li.title = p.cwd || "";
      ul.appendChild(li);
    }
  }

  function msgRow(m) {
    const div = document.createElement("div");
    div.className = "peer-msg";
    div.innerHTML =
      `<div class="pm-head">` +
      `<span class="pm-time">${esc(fmtTime(m.sent_at))}</span>` +
      `<span class="pm-from">${esc(m.from_name || m.from_id)}</span>` +
      `<span class="pm-arrow">→</span>` +
      `<span class="pm-to">${esc(m.to_name || m.to_id)}</span>` +
      `</div>` +
      `<div class="pm-text">${esc(m.text)}</div>`;
    return div;
  }

  function fmtTime(iso) {
    const d = new Date(iso);
    if (isNaN(d)) return "";
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }

  $("peers-close").onclick = close;
  $("peers-modal").onclick = (e) => { if (e.target.id === "peers-modal") close(); };
  window.ccmuxPeers = { open };
})();
