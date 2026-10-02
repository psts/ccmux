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
  // Bumped by every open, close and history read. A read that lands after any
  // of them writes nothing: a slow answer never fills a panel reopened since
  // (on another group or the same one), and an older read never overwrites a
  // newer one.
  let readGen = 0;
  // The pending redial after a drop. Cleared by close() and open(): a timer
  // left from an earlier open would otherwise dial for the next one, before
  // its first read is in (and after a failed one, when no stream may open).
  let redialTimer = null;
  // Whether a history read has succeeded since this open. Before one, a
  // failed read is the panel's error, shown plainly, and the stream is never
  // opened (the Mac shows its error banner and does not listen); after one,
  // it is a possible gap, noted above the list.
  let loaded = false;
  // How many messages a history read asks for, in both lenses (the Mac's
  // PeerMessage.historyLimit): a read that comes back this full may not reach
  // back to what is on screen.
  const HISTORY_LIMIT = 200;

  async function open(g) {
    group = g;
    clearTimeout(redialTimer);
    redialTimer = null;
    readGen++;
    loaded = false;
    note("");
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
      if (r.ok) {
        viewer = await r.json();
        // partial = the daemon federates onto a hub but would not give this page
        // a credential for it, so what follows is the local registry only. A
        // browser holds no credential, which makes this the normal answer on a
        // member host — and the caveat is the whole point of being told.
        busUnknown = viewer.partial === true;
      } else { busUnknown = true; console.warn("peers: /v1/peers/viewer HTTP " + r.status); }
    } catch (e) {
      busUnknown = true;
      console.warn("peers: could not ask which bus to read:", e);
    }
    if (group !== g) return; // closed or reopened while we asked
    // History first, then the stream, as on the Mac: a first read that fails
    // stays the panel's error, with no live rows arriving to hide it.
    if (await refresh()) connect();
  }

  const auth = () => (viewer.token ? { Authorization: "Bearer " + viewer.token } : {});

  // A refusal is not data. Decoded as JSON it yields an object with no messages
  // in it, which renders as silence.
  function refusal(status) {
    if (status === 503) return "ccmuxd can't reach the hub right now — the sessions live there, so this list would be wrong.";
    if (status === 401 || status === 403) return "ccmuxd refused this page's read of the peers bus.";
    return "The peers bus answered HTTP " + status + ".";
  }

  // A failure says which kind it is, in the Mac lens's words: no answer, a
  // refusal, or an answer this page cannot read (the two are out of step).
  async function readJSON(path) {
    let r;
    try { r = await fetch(viewer.bus + path, { headers: auth() }); } catch (e) {
      throw new Error("Cannot reach ccmuxd at " + location.host, { cause: e });
    }
    if (!r.ok) throw new Error(refusal(r.status));
    try { return await r.json(); } catch (e) {
      throw new Error("ccmuxd answered, but this page can't read the reply; the page and the daemon may be out of step", { cause: e });
    }
  }

  function close() {
    group = null;
    clearTimeout(redialTimer);
    redialTimer = null;
    readGen++;
    if (sock) { sock.close(); sock = null; }
    $("peers-modal").classList.add("hidden");
  }

  // A failed read: the list may be missing what was sent while the stream was
  // down. Its own line, not peers-status, because a live message hides
  // peers-status and says nothing about the gap. Cleared by the next good read.
  // The Mac lens shows the same note (PeerMessagesState.historyNote).
  function note(text) {
    $("peers-note").textContent = text;
    $("peers-note").classList.toggle("hidden", !text);
  }

  async function refresh() {
    if (!group) return;
    const gen = ++readGen;
    const q = "group=" + encodeURIComponent(group);
    let msgs = [], peers = [];
    try {
      [msgs, peers] = await Promise.all([
        readJSON("/v1/peers/messages?" + q + "&limit=" + HISTORY_LIMIT),
        readJSON("/v1/peers?" + q),
      ]);
    } catch (e) {
      if (gen !== readGen) return;
      console.warn("peers: history read failed:", e, e.cause);
      if (!loaded) {
        // Nothing retries a failed open (no stream, so no hello), so say the
        // way out, as the Mac's banner does.
        $("peers-status").textContent = e.message + " Close and reopen to try again.";
        $("peers-status").classList.remove("hidden");
      } else {
        note("Couldn't read back the history (" + e.message + "). Messages sent while disconnected may be missing; reopen to reload.");
      }
      return;
    }
    if (gen !== readGen) return; // closed, reopened or read again meanwhile
    loaded = true;
    note("");
    renderPeers(peers || []);
    mergeRows(msgs || []);
    const box = $("peers-msgs");
    // Rows on screen do not make an unconfirmed bus confirmed: a member host's
    // local registry usually holds stale pre-federation history, which would
    // otherwise render as the hub's with no caveat at all. So the hedge is
    // driven by what we know, not by whether the list came back empty.
    if (busUnknown) {
      $("peers-status").textContent = "Couldn't confirm which bus to read — this may not be the whole picture.";
      $("peers-status").classList.remove("hidden");
    } else if (box.children.length === 0) {
      $("peers-status").textContent = "No messages yet.";
      $("peers-status").classList.remove("hidden");
    } else {
      $("peers-status").classList.add("hidden");
    }
    box.scrollTop = box.scrollHeight;
    return true;
  }

  // History merged into the rows on screen by message number (history's id is
  // the seq a live frame carries): a message that arrived live while the read
  // was in flight is neither lost nor doubled. A row on screen older than the
  // history stays, unless the read came back full: then more may have been
  // sent than it covers, and keeping those rows would draw an unmarked hole
  // between them and the history, so the list becomes what a fresh open
  // shows. Rows with no number keep their order at the end. The Mac lens
  // merges the same way (PeerMessage.merged).
  function mergeRows(msgs) {
    const box = $("peers-msgs");
    const floor = msgs.length >= HISTORY_LIMIT ? Math.min(...msgs.map((m) => m.id)) : 0;
    const bySeq = new Map();
    const loose = [];
    for (const el of box.children) {
      const seq = Number(el.dataset.seq);
      if (!(seq > 0)) loose.push(el);
      else if (seq >= floor) bySeq.set(seq, el);
    }
    for (const m of msgs) bySeq.set(m.id, msgRow(m));
    const rows = [...bySeq.entries()].sort((a, b) => a[0] - b[0]).map((e) => e[1]);
    box.replaceChildren(...rows, ...loose);
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
      // Every hello reads history back and merges it in. The hello is written
      // only after the listener is registered, and the daemon saves a message
      // before it broadcasts it, so nothing falls between that read and the
      // live stream: not what a drop skipped, not what was sent while the
      // panel opened. The Mac lens does the same (PeerMessagesState.readBack).
      if (m.type === "hello") { refresh(); return; }
      if (m.type !== "message") return;
      const box = $("peers-msgs");
      if (m.seq && box.querySelector(`[data-seq="${m.seq}"]`)) return; // read back before its live copy landed
      $("peers-status").classList.add("hidden");
      const follow = box.scrollTop + box.clientHeight >= box.scrollHeight - 30;
      box.appendChild(msgRow(m));
      if (follow) box.scrollTop = box.scrollHeight;
    };
    ws.onclose = () => {
      if (ws !== sock) return; // superseded or modal closed
      sock = null;
      redialTimer = setTimeout(() => { redialTimer = null; if (group) connect(); }, 2000);
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
    div.dataset.seq = String(m.seq ?? m.id ?? ""); // live frames carry seq, history rows id: the same number
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
