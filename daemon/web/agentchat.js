// Agent chat view: an agent pane's conversation as a transcript with a
// prompt box, shown in place of its terminal. The daemon reads opencode's
// server and hands this one normalized stream (/v1/panes/{id}/agent/ws:
// hello, turn, part, delta, idle, permission, state, error); typing into an
// asleep agent wakes it with the text. The raw terminal stays one click
// away ("Terminal"), per pane, for when the TUI itself is what you want.
(() => {
  const modes = new Map(); // paneId → "chat" | "terminal"
  let cur = null; // the pane being shown: { paneId, wsRec, sock, turns, order, els, perms, busy }
  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  };

  function build() {
    const box = $("agent-chat");
    if (box.dataset.built) return box;
    box.dataset.built = "1";
    box.innerHTML =
      `<div class="ac-head"><span class="ac-title"></span><span class="ac-state"></span><span class="ac-session"></span>` +
      `<span class="ac-spacer"></span><button class="ac-stop" title="Stop the current turn">Stop</button>` +
      `<button class="ac-term" title="Show the raw terminal">Terminal</button></div>` +
      `<div class="ac-error hidden"></div><div class="ac-log"></div><div class="ac-perms"></div>` +
      `<form class="ac-compose"><textarea rows="2"></textarea><button type="submit">Send</button></form>`;
    box.querySelector(".ac-term").onclick = () => setMode("terminal");
    box.querySelector(".ac-stop").onclick = () => send({ t: "abort" });
    const form = box.querySelector(".ac-compose");
    const ta = form.querySelector("textarea");
    form.onsubmit = (e) => { e.preventDefault(); submit(ta); };
    ta.onkeydown = (e) => {
      if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); submit(ta); }
    };
    const bar = $("agent-chat-bar");
    bar.innerHTML = `<span class="ac-bar-label"></span><button class="ac-bar-chat">Chat</button>`;
    bar.querySelector(".ac-bar-chat").onclick = () => setMode("chat");
    return box;
  }

  function submit(ta) {
    const text = ta.value.trim();
    if (!text || !cur) return;
    ta.value = "";
    setBusy(true);
    send({ t: "prompt", text });
  }

  function send(frame) {
    if (cur && cur.sock && cur.sock.readyState === WebSocket.OPEN) cur.sock.send(JSON.stringify(frame));
  }

  // show puts the chat (or, in terminal mode, its one-line bar) on screen
  // for an agent pane; a repeat call for the same pane is a no-op so a
  // registry refresh never wipes a draft or the scroll position.
  function show(pane, wsRec) {
    build();
    if (cur && cur.paneId === pane.id) { applyMode(); return; }
    close();
    cur = { paneId: pane.id, wsRec, sock: null, turns: new Map(), order: [], els: new Map(), perms: new Map(), busy: false, agent: pane.agent };
    $("agent-chat").querySelector(".ac-title").textContent = "⚙ " + pane.agent;
    $("agent-chat-bar").querySelector(".ac-bar-label").textContent = "⚙ " + pane.agent + " · terminal";
    setState("connecting", "");
    $("agent-chat").querySelector(".ac-log").innerHTML = "";
    $("agent-chat").querySelector(".ac-perms").innerHTML = "";
    applyMode();
    connect();
  }

  function hide() {
    close();
    cur = null;
    $("agent-chat").classList.add("hidden");
    $("agent-chat-bar").classList.add("hidden");
    $("terminal").classList.remove("hidden");
  }

  function close() {
    if (cur && cur.sock) { const s = cur.sock; cur.sock = null; s.close(); }
  }

  function setMode(mode) {
    if (!cur) return;
    modes.set(cur.paneId, mode);
    applyMode();
  }

  function applyMode() {
    const chat = (modes.get(cur.paneId) || "chat") === "chat";
    $("agent-chat").classList.toggle("hidden", !chat);
    $("agent-chat-bar").classList.toggle("hidden", chat);
    $("terminal").classList.toggle("hidden", chat);
    if (chat) {
      const log = $("agent-chat").querySelector(".ac-log");
      log.scrollTop = log.scrollHeight;
      $("agent-chat").querySelector("textarea").focus();
    } else {
      scheduleFit(); // the terminal got its height back; nothing observes that for it
    }
  }

  function connect() {
    if (!cur) return;
    const { paneId, wsRec } = cur;
    const sock = new WebSocket(`${attachOrigin(wsRec)}/v1/panes/${encodeURIComponent(paneId)}/agent/ws`);
    cur.sock = sock;
    sock.onmessage = (ev) => {
      let f;
      try { f = JSON.parse(ev.data); } catch (_) { return; }
      if (cur && cur.sock === sock) apply(f);
    };
    sock.onclose = () => {
      if (!cur || cur.sock !== sock) return; // superseded or hidden
      cur.sock = null;
      setState("reconnecting", "");
      setTimeout(() => { if (cur && cur.paneId === paneId && !cur.sock) connect(); }, 2000);
    };
  }

  function apply(f) {
    switch (f.t) {
      case "hello": return hello(f);
      case "state": return setState(f.state, null);
      case "turn": return upsertTurn(f.turn);
      case "part": return upsertPart(f.messageId, f.part);
      case "delta": return delta(f);
      case "idle": return setBusy(false);
      case "permission": return addPermission(f.permission);
      case "permission-replied": return removePermission(f.id);
      case "error": return showError(f.error);
    }
  }

  function hello(f) {
    const box = $("agent-chat");
    cur.turns = new Map(); cur.order = []; cur.els = new Map(); cur.perms = new Map();
    box.querySelector(".ac-log").innerHTML = "";
    box.querySelector(".ac-perms").innerHTML = "";
    showError(f.error || "");
    setState(f.state, f.title || "");
    for (const t of f.turns || []) upsertTurn(t);
    for (const p of f.permissions || []) addPermission(p);
    setBusy(false);
    const log = box.querySelector(".ac-log");
    log.scrollTop = log.scrollHeight;
  }

  function setState(state, title) {
    const box = $("agent-chat");
    cur.state = state;
    box.querySelector(".ac-state").textContent = state;
    box.querySelector(".ac-state").dataset.state = state;
    if (title != null) box.querySelector(".ac-session").textContent = title;
    box.querySelector(".ac-stop").classList.toggle("hidden", state !== "running" || !cur.busy);
    box.querySelector("textarea").placeholder =
      state === "asleep" ? `Message ${cur.agent}… Enter starts it` : `Message ${cur.agent}… Enter sends, Shift+Enter for a new line`;
  }

  function setBusy(busy) {
    if (!cur) return;
    cur.busy = busy;
    $("agent-chat").querySelector(".ac-stop").classList.toggle("hidden", !(busy && cur.state === "running"));
    $("agent-chat").querySelector(".ac-log").classList.toggle("busy", busy);
  }

  function showError(text) {
    const e = $("agent-chat").querySelector(".ac-error");
    e.textContent = text;
    e.classList.toggle("hidden", !text);
  }

  // --- transcript ---
  function upsertTurn(t) {
    const log = $("agent-chat").querySelector(".ac-log");
    const follow = log.scrollTop + log.clientHeight >= log.scrollHeight - 40;
    let rec = cur.turns.get(t.id);
    if (!rec) {
      rec = { turn: t, el: el("div", "ac-turn " + t.role) };
      rec.el.appendChild(el("div", "ac-who", t.role === "user" ? "you" : cur.agent));
      rec.parts = el("div", "ac-parts");
      rec.el.appendChild(rec.parts);
      rec.err = el("div", "ac-turn-error hidden");
      rec.el.appendChild(rec.err);
      cur.turns.set(t.id, rec);
      cur.order.push(t.id);
      log.appendChild(rec.el);
    }
    rec.turn = Object.assign(rec.turn, t, { parts: rec.turn.parts });
    rec.err.textContent = t.error || "";
    rec.err.classList.toggle("hidden", !t.error);
    for (const p of t.parts || []) upsertPart(t.id, p);
    if (t.role === "assistant" && !t.error) setBusy(true);
    if (follow) log.scrollTop = log.scrollHeight;
  }

  function upsertPart(messageId, p) {
    if (!cur.turns.has(messageId)) upsertTurn({ id: messageId, role: "assistant", parts: [] });
    const rec = cur.turns.get(messageId);
    let pe = cur.els.get(p.id);
    if (!pe) {
      pe = partEl(p);
      cur.els.set(p.id, pe);
      rec.parts.appendChild(pe.root);
    }
    pe.update(p);
    const log = $("agent-chat").querySelector(".ac-log");
    if (log.scrollTop + log.clientHeight >= log.scrollHeight - 80) log.scrollTop = log.scrollHeight;
  }

  function delta(f) {
    const pe = cur.els.get(f.partId);
    if (!pe) { upsertPart(f.messageId, { id: f.partId, type: f.field === "text" ? "text" : "reasoning", text: f.delta }); return; }
    pe.append(f.field, f.delta);
  }

  // partEl renders one part and knows how to update it in place.
  function partEl(p) {
    if (p.type === "tool") return toolEl(p);
    const root = p.type === "reasoning" ? el("details", "ac-reason") : el("div", "ac-text");
    let body = root;
    if (p.type === "reasoning") {
      root.appendChild(el("summary", "", "Thought"));
      body = el("div", "ac-reason-text");
      root.appendChild(body);
    }
    return {
      root,
      update: (np) => { body.textContent = np.text || ""; },
      append: (field, d) => { if (field === "text") body.textContent += d; },
    };
  }

  const toolMarks = { pending: "○", running: "◐", completed: "●", error: "✖" };
  function toolEl(p) {
    const root = el("details", "ac-tool");
    const summary = el("summary");
    root.appendChild(summary);
    const input = el("pre", "ac-tool-input");
    const output = el("pre", "ac-tool-output");
    root.appendChild(input); root.appendChild(output);
    const update = (np) => {
      root.dataset.status = np.status || "";
      summary.textContent = `${toolMarks[np.status] || "○"} ${np.tool}${np.title ? " · " + np.title : ""}`;
      input.textContent = np.input ? pretty(np.input) : "";
      output.textContent = np.error || np.output || "";
      output.classList.toggle("error", !!np.error);
    };
    return { root, update, append: (field, d) => { if (field === "output") output.textContent += d; } };
  }

  function pretty(v) {
    try { return JSON.stringify(typeof v === "string" ? JSON.parse(v) : v, null, 1); } catch (_) { return String(v); }
  }

  // --- permissions: opencode asking before a tool runs ---
  function addPermission(req) {
    if (!req || cur.perms.has(req.id)) return;
    const card = el("div", "ac-perm");
    card.appendChild(el("div", "ac-perm-text", `${cur.agent} asks to run ${req.permission}: ${(req.patterns || []).join(", ") || "(no pattern)"}`));
    const row = el("div", "ac-perm-actions");
    for (const [label, reply] of [["Allow once", "once"], ["Allow always", "always"], ["Reject", "reject"]]) {
      const b = el("button", "ac-perm-btn " + reply, label);
      b.onclick = () => send({ t: "permission", id: req.id, reply });
      row.appendChild(b);
    }
    card.appendChild(row);
    cur.perms.set(req.id, card);
    $("agent-chat").querySelector(".ac-perms").appendChild(card);
  }

  function removePermission(id) {
    const card = cur.perms.get(id);
    if (card) { card.remove(); cur.perms.delete(id); }
  }

  window.ccmuxAgentChat = { show, hide };
})();
