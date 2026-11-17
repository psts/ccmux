// threads.js: the thread page's controller. It reads the daemon's lists
// (windows, repos, runner sessions), follows the picked session's stream,
// and wires the header, the composer and the cards to the session routes.
// What things mean lives in the pure modules beside it (threadmodel,
// threadstream, threadwork, threadmd, threadimage), what they look like in
// threadview.js; this file is the glue, and owns the page's state.
(() => {
  "use strict";
  const R = window.ccmuxRoute, Model = window.ccmuxThreadModel, Stream = window.ccmuxThreadStream;
  const Work = window.ccmuxThreadWork, View = window.ccmuxThreadView, Img = window.ccmuxThreadImage;
  const $ = (id) => document.getElementById(id);
  const show = (id, on) => $(id).classList.toggle("hidden", !on);

  const S = {
    windows: [], workspaces: [], sessions: [], listed: false,
    sel: R.parseThread(location.search), // what is on screen: a session, a draft, or nothing
    stream: Stream.fresh(), sock: null, retry: window.ccmuxRetry.backoff(), feedRetry: window.ccmuxRetry.backoff(),
    drawn: new Map(), drawnFor: "", cardsSig: "", pinned: true, open: new Set(), file: "", dirty: false, jump: false,
    user: "anon", feed: null, acted: "", images: [], sending: false, error: "", listError: "", menu: false,
    draftModel: "", picked: "", later: new Set(), awaitBoot: null, restarted: false,
  };

  // --- the daemon ---

  async function call(method, path, body) {
    const init = { method, headers: {} };
    if (body instanceof Blob) init.body = body;
    else if (body !== undefined) { init.headers["Content-Type"] = "application/json"; init.body = JSON.stringify(body); }
    const r = await fetch(path, init);
    const data = r.status === 204 ? {} : await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(data.error || `HTTP ${r.status}`);
    return data;
  }

  // loadLists reads what the sidebar shows. A list that cannot be read keeps
  // its last value and says so; the others still draw. The complaint is this
  // read's alone: the next read that works takes it down, so a daemon that
  // was away for a moment does not leave "failed to fetch" on screen.
  async function loadLists() {
    let failed = "";
    const read = (path, pick) => call("GET", path).then(pick, (e) => { failed = `${path}: ${e.message}`; return null; });
    const [windows, workspaces, sessions] = await Promise.all([
      read("/v1/windows", (d) => d.windows || d || []), read("/v1/workspaces", (d) => d || []), read("/v1/sessions", (d) => d.sessions || []),
    ]);
    if (windows) S.windows = windows;
    if (workspaces) S.workspaces = workspaces;
    if (sessions) S.sessions = sessions;
    S.listError = failed;
    S.listed = true;
    drawAll();
    maybeActed();
  }

  const session = () => (S.sel.kind === "session" ? S.sessions.find((s) => s.id === S.sel.id) || null : null);
  const workspace = () => {
    const sess = session();
    const id = sess ? sess.workspaceId : S.sel.kind === "draft" ? S.sel.ws : "";
    return S.workspaces.find((w) => w.id === id) || null;
  };
  const groupOf = (ws) => { const w = ws && S.windows.find((x) => (x.workspaceIds || []).includes(ws.id)); return w ? w.name : "Other"; };

  // --- what is on screen ---

  function select(sel, push = true) {
    S.sel = sel;
    if (push) history.pushState(null, "", R.threadHref(sel));
    S.stream = Stream.fresh();
    S.drawn.clear();
    $("blocks").textContent = "";
    S.cardsSig = "";
    S.open.clear();
    Object.assign(S, { file: "", error: "", restarted: false, awaitBoot: null, picked: "", menu: false });
    closeSock();
    if (sel.kind === "session") openSock();
    document.getElementById("page").classList.remove("side-open");
    drawAll();
    maybeActed();
  }

  function closeSock() {
    const s = S.sock;
    S.sock = null;
    if (s) s.close();
  }

  // openSock dials the picked session's stream from where the page stands:
  // the runner process it was reading and the last event it has.
  function openSock() {
    const id = S.sel.id;
    const at = Stream.cursor(S.stream);
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const sock = new WebSocket(`${proto}://${location.host}/v1/sessions/${encodeURIComponent(id)}/ws?boot=${encodeURIComponent(at.boot)}&after=${at.after}`);
    S.sock = sock;
    sock.onopen = () => S.retry.ok();
    sock.onmessage = (ev) => { if (S.sock === sock) onFrame(JSON.parse(ev.data)); };
    sock.onclose = () => {
      if (S.sock !== sock) return; // closed on purpose, or another session was picked
      S.sock = null;
      setTimeout(() => { if (!S.sock && S.sel.kind === "session" && S.sel.id === id) openSock(); }, S.retry.next());
    };
  }

  function onFrame(frame) {
    const out = Stream.apply(S.stream, frame, Date.now());
    S.stream = out.state;
    if (out.resync) {
      // Something was missed: forget the place and dial again for a snapshot.
      S.stream.boot = "";
      S.stream.seq = 0;
      closeSock();
      openSock();
      return;
    }
    if (out.all) afterSnapshot();
    schedule();
  }

  // afterSnapshot: a snapshot from another runner process than the one
  // "Restart now" was pressed on is that restart having happened.
  function afterSnapshot() {
    S.drawn.clear();
    $("blocks").textContent = "";
    S.cardsSig = "";
    S.jump = true; // a conversation just loaded opens at its end
    if (S.awaitBoot !== null && S.stream.boot && S.stream.boot !== S.awaitBoot) {
      S.awaitBoot = null;
      S.restarted = true;
    }
  }

  // --- drawing ---

  function schedule() {
    if (S.dirty) return;
    S.dirty = true;
    requestAnimationFrame(() => { S.dirty = false; drawAll(); });
  }

  function drawAll() {
    drawSide();
    drawHead();
    drawBars();
    drawStream();
    drawDetail();
    drawComposer();
  }

  function drawSide() {
    const sess = session();
    const ws = workspace();
    View.sidebar($("tree"), Model.tree(S.windows, S.workspaces, S.sessions), {
      isLead: (r) => (sess ? !!r.lead && r.lead.id === sess.id : S.sel.kind === "draft" && S.sel.as === "lead" && S.sel.ws === r.id),
      isSession: (s) => !!sess && s.id === sess.id,
      inRepo: (r) => !!ws && ws.id === r.id,
      pickRepo: (r) => select(r.lead ? { kind: "session", id: r.lead.id } : { kind: "draft", ws: r.id, as: "lead" }),
      pickSession: (s) => select({ kind: "session", id: s.id }),
    });
    $("new-thread").disabled = !ws;
    const info = S.stream.info || {};
    const foot = $("side-foot");
    foot.textContent = "";
    if (info.claudeCode || S.stream.sdk) foot.appendChild(View.el("span", "", `Claude Code ${info.claudeCode || "?"} · SDK ${S.stream.sdk || "?"}`));
  }

  // liveState is where the picked session is right now: its row's word,
  // sharpened by what its stream just said.
  function liveState(sess) {
    if (S.stream.where === "starting" || S.stream.where === "updating") return S.stream.where;
    const state = Model.stateOf(sess);
    if (state === "ended" || state === "asleep" || state === "unknown") return state;
    if (S.stream.cards.length) return "waiting";
    return S.stream.busy ? "running" : state === "running" ? "idle" : state;
  }

  function headText(sess, ws) {
    const repo = ws ? Model.repoName(ws) : "";
    const group = groupOf(ws);
    if (sess) {
      const lead = sess.kind === "lead";
      const filed = S.sessions.filter((s) => s.workspaceId === sess.workspaceId && s.kind !== "lead" && s.state !== "done").length;
      return { title: Model.titleOf(sess, ws), meta: lead ? `${group} · main session for ${repo} · ${filed} thread${filed === 1 ? "" : "s"} filed under it` : `${group} · lead: ${repo}` };
    }
    if (S.sel.kind === "draft" && ws) {
      const lead = S.sel.as === "lead";
      return { title: lead ? repo : "New thread", meta: `${group} · ${lead ? "main session for " + repo : "a new thread under " + repo} · it starts with your first message` };
    }
    if (S.sel.kind === "none") return { title: "Threads", meta: "" };
    // Named in the address but not in the lists: not read yet, or gone.
    return S.listed ? { title: "Not found", meta: "This session or repo is not on this host any more." } : { title: "…", meta: "" };
  }

  function drawHead() {
    const sess = session();
    const ws = workspace();
    const text = headText(sess, ws);
    $("title").textContent = text.title;
    $("meta").textContent = text.meta;
    document.title = text.title + " · ccmux";
    const state = sess ? liveState(sess) : "";
    const badge = $("state");
    badge.className = "badge " + state + (sess ? "" : " hidden");
    badge.textContent = { starting: "Starting", updating: "Updating" }[state] || Model.stateLabel(state);
    const live = !!sess && sess.state !== "done";
    show("model-box", live || (S.sel.kind === "draft" && !!ws));
    show("stop", live && (S.stream.busy || S.stream.cards.length > 0));
    show("more", live);
    show("rename", !!sess && sess.kind !== "lead");
    drawModel(sess);
  }

  function drawModel(sess) {
    const mv = Model.modelView(sess || { model: S.draftModel }, S.stream.info);
    const select = $("model");
    const sig = JSON.stringify(mv.options);
    if (select.dataset.sig !== sig) {
      select.dataset.sig = sig;
      select.textContent = "";
      for (const o of mv.options) { const opt = View.el("option", "", o.label); opt.value = o.value; select.appendChild(opt); }
    }
    select.value = mv.wanted;
    const pending = !!sess && sess.state !== "done" && (mv.pending || S.picked === sess.id);
    const later = pending && S.later.has(sess.id);
    show("model-bar", pending && !later);
    show("restart-later", later);
    $("restart-later").textContent = `Restart to apply ${mv.wantedName}`;
    $("restart-later").title = `${mv.wantedName} applies on the next restart`;
    const text = $("model-bar-text");
    text.textContent = "";
    text.appendChild(View.el("strong", "", `${mv.wantedName} applies on the next restart.`));
    text.appendChild(document.createTextNode(` This session keeps running on ${mv.runningName || "its current model"} until then.`));
    show("restarted-bar", S.restarted && !pending);
    $("restarted-bar").textContent = `Restarted on ${mv.runningName || mv.wantedName}. The session picked up where it was.`;
  }

  function drawBars() {
    const sess = session();
    const note = S.stream.note || (sess && sess.note) || "";
    show("notice-bar", !!note);
    $("notice-bar").textContent = note;
    const error = S.error || S.stream.error || S.listError;
    show("error-bar", !!error);
    $("error-bar").textContent = error;
    const ws = workspace();
    show("goal", !!sess || S.sel.kind === "draft");
    const repo = ws ? Model.repoName(ws) : "this repo";
    const lead = sess ? sess.kind === "lead" : S.sel.as === "lead";
    $("goal-text").textContent = (sess && sess.goal) || (lead
      ? `The repo's own lead. Ask it anything about ${repo}, or start a thread from here; the thread is filed under ${repo}.`
      : "No goal noted yet. The session writes it here as it works.");
  }

  const viewCtx = () => {
    const sess = session();
    const ws = workspace();
    const repo = ws ? ws.repoPath || "" : "";
    return {
      sessionId: S.sel.kind === "session" ? S.sel.id : "", now: Date.now(), // from the address: the stream can be ahead of the lists
      who: ws ? `Claude · ${Model.repoName(ws)}${sess && sess.kind === "lead" ? " lead" : ""}` : "Claude",
      isOpen: (id) => S.open.has(id),
      toggle: (id) => { if (!S.open.delete(id)) S.open.add(id); schedule(); },
      rel: (path) => Work.rel(path, repo),
      diffFor: (req) => diffOfCall(req.tool && req.tool.callID),
      answer: answerCard,
    };
  };

  // diffOfCall is the change a pending tool call would make, for its card.
  function diffOfCall(callID) {
    if (!callID) return null;
    for (const t of S.stream.turns) for (const p of t.parts) if (p.callID === callID && p.state && p.state.diff) return p.state.diff;
    return null;
  }

  const nearBottom = (box) => box.scrollTop + box.clientHeight >= box.scrollHeight - 80;

  // drawStream draws the blocks that changed, in place: a long conversation
  // is not rebuilt for every word that streams in.
  function drawStream() {
    const ws = workspace();
    const ctx = viewCtx();
    const follow = S.pinned || S.jump;
    S.jump = false;
    const blocks = Work.blocks(S.stream.turns, ws ? ws.repoPath || "" : "", ws ? Model.repoName(ws) : "");
    const box = $("blocks");
    // What every block is drawn with (whose words, which session's images)
    // changed, as when the lists arrive after the conversation: draw anew.
    if (S.drawnFor !== ctx.who + "|" + ctx.sessionId) {
      S.drawnFor = ctx.who + "|" + ctx.sessionId;
      S.drawn.clear();
      box.textContent = "";
    }
    const seen = new Set();
    let at = box.firstChild;
    for (const b of blocks) {
      const key = b.kind + ":" + b.id;
      seen.add(key);
      const sig = View.sig(b, ctx);
      let rec = S.drawn.get(key);
      if (!rec || rec.sig !== sig) {
        const made = View.block(b, ctx);
        if (rec) rec.el.replaceWith(made);
        if (rec && at === rec.el) at = made;
        rec = { el: made, sig };
        S.drawn.set(key, rec);
      }
      if (rec.el !== at) box.insertBefore(rec.el, at);
      at = rec.el.nextSibling;
    }
    for (const [key, rec] of S.drawn) if (!seen.has(key)) { rec.el.remove(); S.drawn.delete(key); }
    drawCards(ctx);
    const picked = S.sel.kind !== "none";
    show("empty", !blocks.length && !S.stream.cards.length);
    $("empty").textContent = !picked ? "Pick a repo on the left to talk to its lead, or start a thread."
      : S.sel.kind === "draft" ? "Nothing here yet. Your first message starts the session." : "No messages yet.";
    if (follow) pinToEnd();
  }

  // pinToEnd keeps the newest of the conversation on screen. The page stays
  // pinned while the reader is at the end, through words streaming in and
  // through a picture that loads late and pushes the end down; scrolling up
  // lets go.
  function pinToEnd() {
    const scroller = $("stream");
    scroller.scrollTop = scroller.scrollHeight;
    S.pinned = true;
  }

  function wireStream() {
    const scroller = $("stream");
    scroller.addEventListener("scroll", () => { S.pinned = nearBottom(scroller); });
    scroller.addEventListener("load", () => { if (S.pinned) pinToEnd(); }, true); // an image's load does not bubble: heard on the way down
  }

  // drawCards rebuilds the cards only when the set of them changed, so a
  // half-typed answer is not wiped by a word streaming in above it.
  function drawCards(ctx) {
    const sig = S.stream.cards.map((c) => c.id).join(",");
    if (sig === S.cardsSig) return;
    S.cardsSig = sig;
    const box = $("cards");
    box.textContent = "";
    for (const c of S.stream.cards) box.appendChild(View.card(c, ctx));
  }

  async function answerCard(card, body) {
    try {
      await call("POST", `/v1/sessions/${S.sel.id}/cards/${encodeURIComponent(card.id)}`, body);
      return "";
    } catch (e) {
      return e.message;
    }
  }

  function drawDetail() {
    const ws = workspace();
    const files = Work.changes(S.stream.turns, ws ? ws.repoPath || "" : "");
    View.changesTab($("tab-changes"), files, S.file, (path) => { S.file = path; schedule(); });
    View.todoTab($("tab-todo"), S.stream.todos, S.stream.result);
  }

  // --- the composer ---

  function drawComposer() {
    const sess = session();
    const usable = (sess && sess.state !== "done") || (S.sel.kind === "draft" && !!workspace());
    show("composer", usable);
    const lead = sess ? sess.kind === "lead" : S.sel.as === "lead";
    document.querySelector("#composer label").textContent = `Message ${lead ? "the lead" : "this thread"}, or type / for commands`;
    $("send").disabled = S.sending;
    $("hint").textContent = S.sending ? "Sending…" : sess && sess.state === "asleep" ? "Asleep. A message wakes it." : "Lands as the next turn if Claude is busy";
    const box = $("attached");
    box.textContent = "";
    S.images.forEach((img, i) => {
      const chip = View.el("div", "chip");
      const pic = View.el("img");
      pic.src = img.url;
      pic.alt = "Image to send";
      const drop = View.el("button", "", "×");
      drop.type = "button";
      drop.title = "Remove";
      drop.onclick = () => { URL.revokeObjectURL(img.url); S.images.splice(i, 1); drawComposer(); };
      chip.append(pic, drop);
      box.appendChild(chip);
    });
    drawCommands();
  }

  function addImages(files) {
    for (const file of files) S.images.push({ file, url: URL.createObjectURL(file) });
    drawComposer();
  }

  // commandsFor is the session's slash commands that fit what is typed,
  // without the ones that only make sense in a terminal.
  function commandsFor(typed) {
    const info = S.stream.info || {};
    const hidden = new Set(info.terminalCommands || []);
    const want = typed.startsWith("/") ? typed.slice(1).toLowerCase() : "";
    const fits = (info.commands || []).filter((c) => !hidden.has(c.name) && c.name.toLowerCase().includes(want));
    const starts = (c) => (c.name.toLowerCase().startsWith(want) ? 0 : 1);
    return fits.sort((a, b) => starts(a) - starts(b)).slice(0, 200); // what was typed leads; the menu scrolls
  }

  function drawCommands() {
    const input = $("input");
    const typing = /^\/\S*$/.test(input.value);
    const box = $("commands");
    show("commands", S.menu || typing);
    if (!S.menu && !typing) return;
    box.textContent = "";
    const list = commandsFor(typing ? input.value : "");
    if (!list.length) box.appendChild(View.el("div", "cmd-none", (S.stream.info.commands || []).length ? "No command matches." : "The commands are listed once the session has started."));
    for (const c of list) {
      const row = View.el("button", "cmd");
      row.type = "button";
      row.append(View.el("span", "cmd-name", "/" + c.name + (c.hint ? " " + c.hint : "")), View.el("span", "cmd-desc", c.description));
      row.onclick = () => { input.value = "/" + c.name + " "; S.menu = false; input.focus(); drawCommands(); };
      box.appendChild(row);
    }
  }

  // ensureSession makes the lead or thread a draft stands for (a lead that
  // is there already is answered with, never made twice) and puts it on
  // screen.
  async function ensureSession() {
    if (S.sel.kind === "session") return S.sel.id;
    const ws = workspace();
    const body = { cwd: ws.repoPath, workspaceId: ws.id, model: S.draftModel };
    if (S.sel.as === "lead") body.kind = "lead";
    const sess = await call("POST", "/v1/sessions", body);
    S.sessions = S.sessions.filter((s) => s.id !== sess.id).concat(sess);
    select({ kind: "session", id: sess.id });
    return sess.id;
  }

  async function uploadImages(id) {
    const names = [];
    for (const img of S.images) {
      const blob = await Img.prepare(img.file);
      names.push((await call("POST", `/v1/sessions/${id}/images`, blob)).name);
    }
    return names;
  }

  // send makes the session if this is its first message, uploads the
  // pictures, then queues the prompt. Whatever fails, the text and the
  // pictures stay in the composer for the retry.
  async function send() {
    const input = $("input");
    const text = input.value.trim();
    if (S.sending || (!text && !S.images.length)) return;
    S.sending = true;
    S.error = "";
    const images = S.images;
    drawComposer();
    try {
      const id = await ensureSession();
      S.images = images; // picking the new session cleared nothing of the composer, keep it so
      const names = await uploadImages(id);
      await call("POST", `/v1/sessions/${id}/prompt`, { text, sender: S.user, images: names });
      input.value = "";
      S.images.forEach((img) => URL.revokeObjectURL(img.url));
      S.images = [];
      S.menu = false;
    } catch (e) {
      S.error = "Not sent: " + e.message;
    }
    S.sending = false;
    drawAll();
  }

  // --- the header's actions ---

  // act runs one session action and says why when it fails.
  async function act(work) {
    S.error = "";
    try {
      await work(session());
    } catch (e) {
      S.error = e.message;
    }
    drawAll();
  }

  async function pickModel(value) {
    const sess = session();
    if (!sess) { S.draftModel = value; return; }
    await act(async () => {
      await call("POST", `/v1/sessions/${sess.id}/model`, { model: value });
      sess.model = value;
      S.picked = sess.id;
      S.later.delete(sess.id);
      S.restarted = false;
    });
  }

  const restartNow = () => act(async (sess) => {
    await call("POST", `/v1/sessions/${sess.id}/restart`);
    S.awaitBoot = S.stream.boot;
    S.picked = "";
    S.later.delete(sess.id);
  });

  const setWanted = (wanted) => act(async (sess) => {
    await call("POST", `/v1/sessions/${sess.id}/wanted`, { wanted });
    $("more").open = false;
    await loadLists();
    if (wanted === "done") select({ kind: "none" });
  });

  const rename = () => act(async (sess) => {
    $("more").open = false;
    const title = prompt("Name this thread:", sess.title || "");
    if (title === null) return;
    sess.title = (await call("POST", `/v1/sessions/${sess.id}/title`, { title })).title;
  });

  // maybeActed tells the daemon a human has seen a finished turn: the picked
  // session's claim is retired everywhere once it has been on screen.
  function maybeActed() {
    const sess = session();
    if (!sess || document.visibilityState !== "visible" || Model.stateOf(sess) !== "done") return;
    const key = sess.id + ":" + (sess.attentionSince || 0);
    if (S.acted === key) return;
    S.acted = key;
    call("POST", `/v1/sessions/${sess.id}/acted`).catch((e) => console.warn("threads: acted not sent:", e.message));
  }

  // --- the firehose: attention for the dots, and a nudge to read the lists ---

  let reload = 0;
  const reloadSoon = () => { clearTimeout(reload); reload = setTimeout(loadLists, 250); };

  function noteAttention(e) {
    const sess = S.sessions.find((s) => s.paneId && s.paneId === e.pane);
    if (!sess) return false;
    sess.attention = e.state;
    sess.attentionSince = e.since || 0;
    return true;
  }

  const feedHandlers = new Map([
    ["hello", (m) => (m.attention || []).map(noteAttention).some(Boolean)],
    ["attention", noteAttention],
    ["sessions-changed", reloadSoon], ["workspace-added", reloadSoon], ["workspace-removed", reloadSoon],
    ["workspace-status", reloadSoon], ["workspace-git", reloadSoon],
  ]);

  const reportPresence = () => {
    if (S.feed && S.feed.readyState === 1) S.feed.send(JSON.stringify({ t: "focus", present: document.visibilityState === "visible" }));
  };

  function connectFeed() {
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const feed = new WebSocket(`${proto}://${location.host}/v1/events?user=${encodeURIComponent(S.user)}&device=web`);
    S.feed = feed;
    feed.onopen = () => { S.feedRetry.ok(); reportPresence(); };
    feed.onmessage = (ev) => {
      const m = JSON.parse(ev.data);
      const handle = feedHandlers.get(m.t);
      if (handle && handle(m) === true) { schedule(); maybeActed(); }
    };
    feed.onclose = () => { S.feed = null; setTimeout(connectFeed, S.feedRetry.next()); };
  }

  // identity is the name this page reports presence under and signs prompts
  // with: the one the daemon vouches for, else the one the lens stored.
  async function identity() {
    let stored = "";
    try { stored = localStorage.getItem("ccmux-user") || ""; } catch (_) { /* storage blocked: anon */ }
    try {
      const id = await call("GET", "/v1/whoami?user=" + encodeURIComponent(stored));
      if (id.vouched && id.login) return id.display || id.login;
    } catch (_) { /* an older daemon, or unreachable: the stored name stands */ }
    return stored || "anon";
  }

  // --- wiring ---

  const editable = (t) => !!t && (t.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(t.tagName));

  function onKey(e) {
    if (e.key === "Escape" && S.menu) { S.menu = false; drawCommands(); return; }
    if (e.metaKey || e.ctrlKey || e.altKey || editable(e.target)) return; // a digit typed into the composer is a digit
    if ("1234".includes(e.key) && View.pressKey($("cards"), e.key)) e.preventDefault();
  }

  function wireComposer() {
    const input = $("input");
    $("composer").onsubmit = (e) => { e.preventDefault(); send(); };
    input.onkeydown = (e) => { if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); } };
    input.oninput = () => { S.menu = false; drawCommands(); };
    input.onpaste = (e) => {
      const files = Img.pasted(e.clipboardData);
      if (!files.length) return; // text pastes as text
      e.preventDefault();
      addImages(files);
    };
    $("attach").onclick = () => $("file").click();
    $("file").onchange = (e) => { addImages(Img.pasted({ files: e.target.files })); e.target.value = ""; };
    $("slash").onclick = () => { S.menu = !S.menu; drawCommands(); };
  }

  function wireHead() {
    $("model").onchange = (e) => pickModel(e.target.value);
    $("restart-now").onclick = restartNow;
    $("restart-later").onclick = restartNow;
    $("later").onclick = () => { const sess = session(); if (sess) S.later.add(sess.id); drawAll(); };
    $("stop").onclick = () => act((sess) => call("POST", `/v1/sessions/${sess.id}/abort`));
    $("sleep").onclick = () => setWanted("asleep");
    $("end").onclick = () => { if (confirm("End this session? Its conversation stays on disk, but it will not run again.")) setWanted("done"); };
    $("rename").onclick = rename;
    $("new-thread").onclick = () => { const ws = workspace(); if (ws) select({ kind: "draft", ws: ws.id, as: "thread" }); };
    $("side-toggle").onclick = () => $("page").classList.toggle("side-open");
    for (const tab of document.querySelectorAll(".tab")) {
      tab.onclick = () => {
        for (const t of document.querySelectorAll(".tab")) t.classList.toggle("on", t === tab);
        show("tab-changes", tab.dataset.tab === "changes");
        show("tab-todo", tab.dataset.tab === "todo");
      };
    }
  }

  async function boot() {
    wireComposer();
    wireHead();
    wireStream();
    document.addEventListener("keydown", onKey);
    document.addEventListener("visibilitychange", () => { reportPresence(); maybeActed(); });
    window.addEventListener("popstate", () => select(R.parseThread(location.search), false));
    // A notification tap that lands on this page names where to go.
    if (navigator.serviceWorker) navigator.serviceWorker.addEventListener("message", (e) => { if (e.data && e.data.type === "ccmux-navigate" && e.data.url) location.href = e.data.url; });
    S.user = await identity();
    connectFeed();
    if (S.sel.kind === "session") openSock();
    await loadLists();
    setInterval(loadLists, 20000);
  }

  boot();
})();
