// Read-only peer-messages viewer for a window group: history + live stream
// from the daemon's built-in peers bus (/v1/peers/*). Opened from a group
// header's chat button; strictly a viewer — sending stays in the sessions.
"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const esc = (s) => String(s).replace(/[<>&]/g, (c) => ({ "<": "&lt;", ">": "&gt;", "&": "&amp;" }[c]));

  let group = null; // open group, null when the modal is closed
  let sock = null;

  function open(g) {
    group = g;
    $("peers-title").textContent = "Messages — " + g.toUpperCase();
    $("peers-modal").classList.remove("hidden");
    $("peers-status").textContent = "Loading…";
    $("peers-status").classList.remove("hidden");
    $("peers-list").innerHTML = "";
    $("peers-msgs").innerHTML = "";
    refresh();
    connect();
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
        fetch("/v1/peers/messages?" + q + "&limit=200").then((r) => r.json()),
        fetch("/v1/peers?" + q).then((r) => r.json()),
      ]);
    } catch (e) {
      $("peers-status").textContent = "Couldn't load messages: " + e.message;
      return;
    }
    renderPeers(peers || []);
    const box = $("peers-msgs");
    box.innerHTML = "";
    for (const m of msgs || []) box.appendChild(msgRow(m));
    $("peers-status").classList.toggle("hidden", (msgs || []).length > 0);
    if ((msgs || []).length === 0) $("peers-status").textContent = "No messages yet.";
    box.scrollTop = box.scrollHeight;
  }

  function connect() {
    if (sock) { sock.close(); sock = null; }
    if (!group) return;
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const ws = new WebSocket(`${proto}://${location.host}/v1/peers/ws?mode=listen&group=${encodeURIComponent(group)}`);
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
