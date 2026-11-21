// threads.js: the thread page's controller. It reads the daemon's lists
// (windows, repos, runner sessions), follows the picked session's stream,
// and wires the header, the composer and the cards to the session routes.
// What things mean lives in the pure modules beside it (threadmodel,
// threadstream, threadwork, threadmd, threadimage), what they look like in
// threadview.js; this file is the glue, and owns the page's state.
(() => {
  "use strict";
  const R = window.ccmuxRoute, Model = window.ccmuxThreadModel, Stream = window.ccmuxThreadStream, Gauge = window.ccmuxThreadGauge;
  const Work = window.ccmuxThreadWork, View = window.ccmuxThreadView, Img = window.ccmuxThreadImage, Team = window.ccmuxThreadTeam;
  const Pending = window.ccmuxThreadPending, Cmds = window.ccmuxThreadCmds, Advisor = window.ccmuxThreadAdvisor;
  const EarlierView = window.ccmuxThreadEarlier;
  const storage = () => { try { return window.localStorage; } catch (_) { return null; } }; // blocked: nothing kept
  const DETAILS = "ccmux-thread-details";
  const loadDetails = () => { try { const v = JSON.parse(storage().getItem(DETAILS) || "[]"); return new Set(Array.isArray(v) ? v : []); } catch (_) { return new Set(); } };
  const $ = (id) => document.getElementById(id);
  const show = (id, on) => $(id).classList.toggle("hidden", !on);

  const S = {
    windows: [], workspaces: [], sessions: [], threads: [], detail: null, listed: false,
    sel: R.parseThread(location.search), // what is on screen: a session, a draft, or nothing
    stream: Stream.fresh(), sock: null, retry: window.ccmuxRetry.backoff(), feedRetry: window.ccmuxRetry.backoff(),
    drawn: new Map(), drawnFor: "", cardsSig: "", pinned: true, open: new Set(), file: "", dirty: false, jump: false,
    user: "anon", feed: null, acted: "", images: [], pending: [], pendingKey: 0, pumping: false, error: "", listError: "", menu: false,
    draftModel: "", draftAdvisor: "", picked: "", later: new Set(), awaitBoot: null, restarted: false,
    details: loadDetails(), // the repos whose git details are open in the sidebar
    cmds: Cmds.load(storage()), cmdsFrom: null, // each repo's last command list, for its drafts (threadcmds.js)
  };

  // The Settings window is the one the lens has (settingsmodal.js and the
  // tab files threads.html loads); it owns the Settings button. This page
  // adds its own tab to it, skills and plugins, and keeps the panel a
  // right-click on a repo or group opens (threadcapsctl.js).
  const Settings = window.ccmuxSettings;

  // Each person's own sidebar (threadsidectl.js): the groups in their
  // order, folded or not, kept by the daemon per login. Its group entries
  // (Move up/down, Open alone) lead the group's right-click menu.
  const Side = window.ccmuxThreadSideCtl.create({
    call, user: () => S.user, changed: () => { if (S.listed) schedule(); }, tree: () => $("tree"),
    taskAlerts: (g) => (g.id ? Tasks.sidebar.taskSum(g).alert || 0 : 0), threadsPath: R.THREADS,
  });
  // The sidebar's "+" (threadnewrepo.js) adds a repo, in a group it picks
  // or makes; a repo's "Move to group ›" and a group's "Add a repo here…"
  // are threadgroupsctl.js's. Both end in the lists being read again.
  const NewRepo = window.ccmuxThreadNewRepo.create({
    call, windows: () => S.windows, changed: () => loadLists(), overlay: () => $("repo-overlay"), sheet: () => $("repo-sheet"),
    opened: (wsId) => select({ kind: "draft", ws: wsId, as: "lead" }),
  });
  const Groups = window.ccmuxThreadGroupsCtl.create({
    call, windows: () => S.windows, changed: () => loadLists(), confirm: (q) => confirm(q), prompt: (q) => prompt(q),
    fail: (text) => { S.error = text; drawAll(); }, addRepo: (group) => NewRepo.open(group),
    openTasks: (win, r) => Tasks.sidebar.repoTasks({ id: win.id }, r).length,
  });
  // A repo's "Hostnames…" (threadhostnamesctl.js): its dev host names, and
  // each saved one opens from the menu.
  const Hosts = window.ccmuxThreadHostnamesCtl.create({
    call, workspace: (id) => S.workspaces.find((w) => w.id === id) || null, changed: () => loadLists(),
    overlay: () => $("hosts-overlay"), sheet: () => $("hosts-sheet"), later: (fn, ms) => setTimeout(fn, ms),
  });
  const hostItems = (r) => [["Hostnames…", () => Hosts.open(r)],
    ...window.ccmuxThreadHostnames.links(S.workspaces.find((w) => w.id === r.id)).map((l) => [l.label, () => window.open(l.url, "_blank")])];
  const Caps = window.ccmuxThreadCapsCtl.create({
    call, lists: () => S, changed: () => loadLists(), confirm: (q) => confirm(q), settings: Settings,
    groupItems: (g) => [...Side.menuItems(g), ...Groups.groupItems(g)], repoItems: (r) => [...hostItems(r), ...Groups.repoItems(r)],
  });

  // "Edit" on a sent message (threadedit.js): back to just before it, its
  // words in the box again.
  const Edit = window.ccmuxThreadEdit.create({
    call, busy: () => !!S.stream.busy, confirm: (q) => confirm(q), wait: (ms) => new Promise((r) => setTimeout(r, ms)),
    fill: (text) => { const input = $("input"); input.value = text; input.focus(); },
    done: (text) => { S.said = text; S.error = ""; drawAll(); },
    fail: (text) => { S.error = text; drawAll(); },
  });

  // What a running session does besides talking (threadlive.js): its mode,
  // its background tasks, the usage limit; and "@" file mentions in its box
  // (threadmention.js).
  const Live = window.ccmuxThreadLive.create({
    call, session: () => session(), info: () => S.stream.info, running: () => { const s = session(); return !!s && s.state === "running"; },
    fail: (text) => { S.error = text; drawAll(); },
    clock: (ms) => new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }),
  });
  const Mention = window.ccmuxThreadMention.create({
    input: $("input"), box: $("mentions"), later: (fn, ms) => setTimeout(fn, ms),
    failed: (text) => { S.error = text; drawAll(); },
    files: async (q) => {
      const sess = session();
      if (!sess) return [];
      return (await call("GET", `/v1/sessions/${encodeURIComponent(sess.id)}/files?q=${encodeURIComponent(q)}`)).files || [];
    },
  });

  // An agent's earlier conversations (threadpast.js): one picked takes the
  // conversation's place, read only, until Back to now or another pick.
  const Past = window.ccmuxThreadPast.create({
    call, sessionId: () => (S.sel.kind === "session" ? S.sel.id : ""), changed: () => { S.pinned = true; drawAll(); },
    fail: (text) => { S.error = text; drawAll(); }, snapshot: (frame) => Stream.fromSnapshot(frame, Date.now()),
    menu: (items, ev) => {
      const box = $("caps-menu");
      window.ccmuxThreadCapsView.menu(box, items, window.ccmuxThreadCapsCtl.pointOf(ev), () => box.classList.add("hidden"));
      box.classList.remove("hidden");
    },
  });

  // The conversations a session's clean starts left (threadearlier.js),
  // shown above the one it runs now; and the question that offers a clean
  // start when the prompt cache is cold (threadclean.js).
  const Earlier = EarlierView.create({ call, snapshot: (frame) => Stream.fromSnapshot(frame, Date.now()), changed: () => schedule() });
  const Clean = window.ccmuxThreadClean.create({ dialog: () => $("clean-ask"), say: (text) => { $("clean-text").textContent = text; } });

  // A group's task queue (threadtasks.js) shows in the main area in place
  // of a conversation: opening it leaves the one on screen, and picking
  // anything (select) closes it. A part's builder is a session, so its
  // conversation opens here, in place of the queue. A worker from before
  // builders were sessions is a terminal, so it opens in the lens.
  const Tasks = window.ccmuxThreadTasks.create({
    user: () => S.user, windows: () => S.windows, changed: () => schedule(),
    leave: () => { Todo.close(); pick({ kind: "none" }, S.sel.kind !== "none"); },
    openSession: (wsId, _paneId, sessionId) => {
      if (sessionId) select({ kind: "session", id: sessionId });
      else window.open(R.href(R.windowOf(S.windows, wsId), wsId), "_blank");
    },
  });

  // A repo's to-do list (threadtodoview.js) takes the main area the same
  // way, and the two never show together: opening one closes the other.
  const Todo = window.ccmuxThreadTodoView.create({
    user: () => S.user, model: () => S.draftModel, call, changed: () => schedule(), confirm: (q) => confirm(q),
    leave: () => { Tasks.close(); pick({ kind: "none" }, S.sel.kind !== "none"); },
  });

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
    rememberCommands();
    drawAll();
    Side.load();
    Caps.listsChanged();
    maybeActed();
    loadTeam();
  }

  // loadTeam reads what only a thread with helpers has: the threads' own
  // rows (a merge shows there) and, for the thread on screen, each member's
  // changes. A daemon with no threads, or a read that fails, costs the page
  // those lines and nothing else: the sessions already say who is in which
  // thread.
  async function loadTeam() {
    const rows = await call("GET", "/v1/threads").then((d) => d.threads || [], () => null);
    if (rows) S.threads = rows;
    const sess = session();
    const id = Team.teamId(sess, S.threads);
    const detail = id ? await call("GET", `/v1/threads/${id}`).then((d) => d, () => null) : null;
    if (Team.teamId(session(), S.threads) !== id) return; // another session was picked meanwhile
    S.detail = detail && detail.thread ? detail : null;
    schedule();
    watchMerge();
  }

  // mergeNow is the merge job of the thread on screen (threads.MergeView),
  // null when it has none to show.
  function mergeNow() {
    const sess = session();
    return sess && S.detail && S.detail.thread.id === Team.teamId(sess, S.threads) ? S.detail.merge || null : null;
  }

  // watchMerge reads the thread again in a moment while its merge runs: the
  // merge is a job outside the daemon, and nothing on the feed says which
  // step it has reached. One read is ever waiting, however often this is
  // asked.
  function watchMerge() {
    const line = Team.mergeLine(mergeNow(), Date.now());
    if (!line || !line.running || S.mergeWatch) return;
    S.mergeWatch = true;
    setTimeout(() => { S.mergeWatch = false; loadTeam(); }, 3000);
  }

  // drawMerge is the bar about the thread's merge: running (with Stop), or
  // how the last one that did not land ended.
  function drawMerge() {
    const line = Team.mergeLine(mergeNow(), Date.now());
    $("merge-bar").className = "bar " + (line && !line.running ? "amber" : "blue") + (line ? "" : " hidden");
    $("merge-text").textContent = line ? line.text : "";
    show("stop-merge", !!line && line.running && !line.stopping);
  }

  const session = () => (S.sel.kind === "session" ? S.sessions.find((s) => s.id === S.sel.id) || null : null);
  const workspace = () => {
    const sess = session();
    const id = sess ? sess.workspaceId : S.sel.kind === "draft" ? S.sel.ws : "";
    return S.workspaces.find((w) => w.id === id) || null;
  };
  // roots are the folders the picked session's paths are written without:
  // its repo's, and a thread member's worktree (threadwork.js).
  const roots = () => { const ws = workspace(); return Work.threadRoots(session(), ws ? ws.repoPath || "" : "", S.stream.turns); };
  const groupOf = (ws) => { const w = ws && S.windows.find((x) => (x.workspaceIds || []).includes(ws.id)); return w ? w.name : "Other"; };

  // --- what is on screen ---

  function select(sel, push = true) {
    Tasks.close();
    Todo.close();
    pick(sel, push);
  }

  // pick puts sel on screen: select, minus closing the task queue and the
  // to-do list.
  function pick(sel, push = true) {
    Past.reset();
    S.sel = sel;
    if (push) history.pushState(null, "", Side.keepAlone(R.threadHref(sel)));
    S.stream = Stream.fresh();
    S.drawn.clear();
    $("blocks").textContent = "";
    S.cardsSig = "";
    S.open.clear();
    Object.assign(S, { file: "", error: "", restarted: false, awaitBoot: null, picked: "", menu: false, detail: null, said: "" });
    closeSock();
    if (sel.kind === "session") openSock();
    document.getElementById("page").classList.remove("side-open");
    drawAll();
    maybeActed();
    loadTeam();
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
    rememberCommands();
    schedule();
  }

  // rememberCommands keeps the command list the session on screen said, so
  // a draft of its repo can offer it before its own session starts. Called
  // on every frame and when the lists load (a snapshot can land before the
  // page knows whose session it is); it acts once per new info. It starts
  // from what storage holds now, so another tab's lists are not lost.
  function rememberCommands() {
    const sess = session();
    if (!sess || S.stream.info === S.cmdsFrom) return;
    S.cmdsFrom = S.stream.info;
    const next = Cmds.remember(cmdMemo(), sess.workspaceId, sess.kind === "lead", S.stream.info, Date.now());
    if (next === S.cmds) return;
    S.cmds = next;
    Cmds.save(storage(), next);
  }

  // cmdMemo is the remembered lists: storage's, which other tabs write too,
  // or this page's own when storage holds none (blocked, or nothing yet).
  const cmdMemo = () => { const kept = Cmds.load(storage()); return Object.keys(kept).length ? kept : S.cmds; };

  // cmdInfo is where the picked conversation's commands come from: its
  // session's own word, or for a draft what its repo's sessions said last.
  const cmdInfo = () => (S.sel.kind === "draft" ? Cmds.forDraft(cmdMemo(), S.sel.ws, S.sel.as === "lead") : S.stream.info || {});

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
      loadLists(); // the row now says what the new start loaded
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
    drawLoadout();
    drawBars();
    drawStream();
    drawDetail();
    drawComposer();
    Live.draw();
  }

  // drawSide leaves the sidebar alone while a group is being dragged: a
  // redraw would take away the element the drag started on.
  function drawSide() {
    const sess = session();
    const ws = workspace();
    if (!Side.dragging()) drawTree(sess, ws);
    show("new-thread", !!ws && !ws.agent); // in the header of a repo; an agent's home holds its one session, no threads
    const info = S.stream.info || {};
    $("side-version").textContent = info.claudeCode || S.stream.sdk ? `Claude Code ${info.claudeCode || "?"} · SDK ${S.stream.sdk || "?"}` : "";
  }

  function drawTree(sess, ws) {
    const groups = Team.decorate(Model.tree(S.windows, S.workspaces, S.sessions), S.sessions, S.workspaces, S.threads);
    View.sidebar($("tree"), Side.arrange(groups), {
      isLead: (r) => (sess ? !!r.lead && r.lead.id === sess.id : S.sel.kind === "draft" && S.sel.as === "lead" && S.sel.ws === r.id),
      isSession: (s) => !!sess && s.id === sess.id,
      inRepo: (r) => !!ws && ws.id === r.id,
      detailOpen: (r) => S.details.has(r.id),
      pickRepo,
      pickSession: (s) => select({ kind: "session", id: s.id }),
      openAgent: (a, g) => { location.href = R.href(g.id, a.id); },
      menu: (kind, item, e) => Caps.menu(kind, item, e),
      tasks: Tasks.sidebar,
      todos: Todo.sidebar,
      side: Side.side,
    });
  }

  // pickRepo goes to a repo's lead; a click on the repo the page is already
  // in opens or closes its git details instead, so they never open on the
  // way in. Open details stay open, several at once, and are kept for the
  // next load.
  function pickRepo(r) {
    const here = S.sel.kind === "session" ? !!r.lead && r.lead.id === S.sel.id : S.sel.kind === "draft" && S.sel.as === "lead" && S.sel.ws === r.id;
    if (!here) return select(r.lead ? { kind: "session", id: r.lead.id } : { kind: "draft", ws: r.id, as: "lead" });
    if (S.details.has(r.id)) S.details.delete(r.id);
    else S.details.add(r.id);
    try { storage().setItem(DETAILS, JSON.stringify([...S.details])); } catch (_) { /* blocked: kept for this load */ }
    drawAll();
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
    if (Tasks.isOpen()) return Tasks.title();
    if (Todo.isOpen()) return Todo.title();
    const repo = ws ? Model.repoName(ws) : "";
    const group = groupOf(ws);
    if (sess && sess.threadId) return { title: Model.titleOf(sess, ws), meta: Team.metaLine(sess, S.sessions, S.workspaces, group, S.threads) };
    if (sess && sess.kind === "agent") return { title: Model.titleOf(sess, ws), meta: `${group} · role agent${sess.agentVersion ? " · base " + sess.agentVersion : ""} · a message starts it` };
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
    const agent = !!sess && sess.kind === "agent"; // its base names its model, and its name
    show("model-box", (live && !agent) || (S.sel.kind === "draft" && !!ws));
    show("past", agent);
    show("stop", live && (S.stream.busy || S.stream.cards.length > 0));
    show("more", live);
    show("rename", !!sess && sess.kind !== "lead" && !agent);
    const leads = live && !!sess.threadId && sess.role === "lead";
    show("merge-thread", leads);
    show("end-thread", leads);
    drawModel(sess);
    drawGauge(sess);
  }

  // drawGauge is the header's context and cache gauge for the open session;
  // a tick redraws it so the cache counts down between messages.
  function drawGauge(sess) {
    const box = $("gauge");
    const g = sess && sess.state !== "done" ? Gauge.gauge(S.stream.result, Date.now(), S.stream.busy) : null;
    show("gauge", !!g);
    if (!g) return;
    box.textContent = "";
    g.parts.forEach((p, i) => {
      if (i) box.appendChild(document.createTextNode(" · "));
      box.appendChild(View.el("span", p.cls, p.text));
    });
    box.title = g.title;
  }

  // fillSelect sets a header picker's options (only when they changed, so
  // an open list is not rebuilt under the person) and its value.
  function fillSelect(select, options, value) {
    const sig = JSON.stringify(options);
    if (select.dataset.sig !== sig) {
      select.dataset.sig = sig;
      select.textContent = "";
      for (const o of options) { const opt = View.el("option", "", o.label); opt.value = o.value; select.appendChild(opt); }
    }
    select.value = value;
  }

  // drawModel is the header's Model and Advisor pickers and the bar that
  // says a pick applies on the next restart. Both apply then; one bar and
  // one Restart now serve both.
  function drawModel(sess) {
    const mv = Model.modelView(sess || { model: S.draftModel }, S.stream.info);
    fillSelect($("model"), mv.options, mv.wanted);
    const live = !!sess && sess.state !== "done";
    const av = drawAdvisor(sess, live, mv);
    drawRestartBar(sess, Advisor.restartBar(mv, av, live && (mv.pending || S.picked === sess.id), live && av.pending), mv);
  }

  // drawAdvisor is the Advisor picker, beside the Model one: for a live
  // session that is not a role agent's, and for a new one before it starts.
  function drawAdvisor(sess, live, mv) {
    const draft = !sess && S.sel.kind === "draft" && !!workspace();
    const av = Advisor.advisorView(sess || { advisor: S.draftAdvisor }, sess ? S.stream.info : null, mv.wanted || (S.stream.info && S.stream.info.model));
    show("advisor-box", (live && sess.kind !== "agent") || draft);
    fillSelect($("advisor"), av.options, av.wanted);
    return av;
  }

  // drawRestartBar is "applies on the next restart" with Restart now, or
  // its small button once the person said later, or "Restarted on".
  function drawRestartBar(sess, bar, mv) {
    const later = bar.pending && S.later.has(sess.id);
    show("model-bar", bar.pending && !later);
    show("restart-later", later);
    $("restart-later").textContent = `Restart to apply ${bar.what}`;
    $("restart-later").title = `${bar.what} applies on the next restart`;
    const text = $("model-bar-text");
    text.textContent = "";
    text.appendChild(View.el("strong", "", `${bar.what} applies on the next restart.`));
    text.appendChild(document.createTextNode(` This session ${bar.keeps} until then.`));
    show("restarted-bar", S.restarted && !bar.pending);
    $("restarted-bar").textContent = `Restarted on ${mv.runningName || mv.wantedName}. The session picked up where it was.`;
  }

  // drawLoadout says a running session's skills and plugins moved on since
  // it started (the daemon's loadoutStale). Not while a restart is on its
  // way, and not beside the model's bar, whose Restart now does the same.
  function drawLoadout() {
    const sess = session();
    const waiting = S.awaitBoot !== null || !$("model-bar").classList.contains("hidden");
    show("loadout-bar", !!sess && !!sess.loadoutStale && sess.state !== "done" && !waiting);
  }

  function drawBars() {
    const sess = session();
    // What a thread action just answered, and the session's own notice: one
    // never hides the other.
    const note = [S.said, S.stream.note || (sess && sess.note) || ""].filter(Boolean).join(" ");
    show("notice-bar", !!note);
    $("notice-bar").textContent = note;
    drawMerge();
    show("past-bar", Past.on());
    $("past-text").textContent = Past.bar();
    const error = S.error || S.stream.error || S.listError;
    show("error-bar", !!error);
    $("error-bar").textContent = error;
    // The goal line is only there when the session wrote one (a helper's
    // task); help text in its place said the same thing every time.
    const goal = (sess && sess.goal) || "";
    show("goal", !!goal);
    $("goal-text").textContent = goal;
  }

  const viewCtx = () => {
    const sess = session();
    const ws = workspace();
    const repo = roots();
    return {
      sessionId: S.sel.kind === "session" ? S.sel.id : "", now: Date.now(), // from the address: the stream can be ahead of the lists
      who: ws ? `Claude · ${Model.repoName(ws)}${sess && sess.kind === "lead" ? " lead" : ""}` : "Claude",
      isOpen: (id) => S.open.has(id),
      toggle: (id) => { if (!S.open.delete(id)) S.open.add(id); schedule(); },
      rel: (path) => Work.rel(path, repo),
      diffFor: (req) => diffOfCall(req.tool && req.tool.callID),
      answer: answerCard,
      retry: retryPending,
      drop: dropPending,
      edit: S.sel.kind === "session" && !Past.on() ? (b) => Edit.edit(S.sel.id, b) : null,
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
    const past = Past.turns(); // an earlier conversation read from disk, in place of this one
    const toBlocks = (turns) => Work.blocks(turns, roots(), ws ? Model.repoName(ws) : "");
    const blocks = past ? toBlocks(past)
      : earlierBlocks(toBlocks).concat(toBlocks(S.stream.turns), Pending.blocks(settlePending(), R.threadHref(S.sel)));
    const box = $("blocks");
    // What every block is drawn with (whose words, which session's images,
    // which of its conversations) changed: draw anew.
    if (S.drawnFor !== ctx.who + "|" + ctx.sessionId + "|" + Past.key()) {
      S.drawnFor = ctx.who + "|" + ctx.sessionId + "|" + Past.key();
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
    drawCards(ctx, past ? [] : S.stream.cards);
    const picked = S.sel.kind !== "none";
    show("empty", !blocks.length && !S.stream.cards.length);
    $("empty").textContent = !picked ? "Pick a repo on the left to talk to its lead, or start a thread."
      : S.sel.kind === "draft" ? "Nothing here yet. Your first message starts the session." : "No messages yet.";
    if (follow) pinToEnd();
  }

  // settlePending drops the sent messages the stream now holds, and gives
  // their pictures back to the browser: the conversation draws them from
  // the daemon from here on. What is left is still on its way.
  function settlePending() {
    const { left, landed } = Pending.arrived(S.pending, S.stream.turns);
    landed.forEach((p) => p.urls.forEach((u) => URL.revokeObjectURL(u)));
    S.pending = left;
    return left;
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
  function drawCards(ctx, cards) {
    const sig = cards.map((c) => c.id).join(",");
    if (sig === S.cardsSig) return;
    S.cardsSig = sig;
    const box = $("cards");
    box.textContent = "";
    for (const c of cards) box.appendChild(View.card(c, ctx));
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
    const files = Work.changes(S.stream.turns, roots());
    const sess = session();
    const team = Team.teamId(sess, S.threads);
    const mine = !!sess && !!S.detail && S.detail.thread.id === team;
    View.changesTab($("tab-changes"), files, S.file, (path) => { S.file = path; schedule(); }, mine ? Team.changeGroups(S.detail) : []);
    View.helpersTab($("tab-team"), Team.helperRows(sess, S.sessions, S.workspaces, S.threads), {
      isPicked: (r) => !!sess && r.id === sess.id,
      pick: (r) => select({ kind: "session", id: r.id }),
      side: isSideTeam(team) || (!!sess && sess.kind === "lead" && !sess.threadId),
      canStandDown: !!team && !!sess && (sess.role === "lead" || !sess.threadId),
      standDown: (r) => threadAct("stand-down", { repo: r.name }, standDownAsk(r.name, team)),
    });
    View.stepsTab($("tab-steps"), S.stream.todos, S.stream.result);
  }

  // isSideTeam says team (a thread id) is a main session's side thread.
  function isSideTeam(team) {
    return (S.threads || []).some((t) => t.id === team && t.kind === "side");
  }

  // standDownAsk is the question before a helper is stood down: a main
  // session's helper works in a scratch copy, which is deleted with it; a
  // thread's helper keeps what it has not committed.
  function standDownAsk(name, team) {
    return isSideTeam(team)
      ? `Stand the ${name} helper down? Its scratch copy is deleted; nothing in it is kept.`
      : `Stand the ${name} helper down? Work it has not committed stays in its worktree.`;
  }

  // threadAct runs one action on the picked session's thread (merge, end,
  // stand a helper down) and shows what the daemon said of it: what it did,
  // or why it would not.
  async function threadAct(action, body, ask) {
    const sess = session();
    const team = Team.teamId(sess, S.threads);
    if (!team || (ask && !confirm(ask))) return;
    $("more").open = false;
    await act(async () => { S.said = (await call("POST", `/v1/threads/${team}/${action}`, body)).message || ""; });
    loadLists();
  }

  // --- the composer ---

  function drawComposer() {
    const sess = session();
    const usable = (sess && sess.state !== "done") || (S.sel.kind === "draft" && !!workspace());
    show("composer", usable && !Past.on()); // an earlier conversation is read only
    const lead = sess ? sess.kind === "lead" : S.sel.as === "lead";
    const whom = sess && sess.kind === "agent" ? "this agent" : lead ? "the lead" : "this thread";
    $("input").setAttribute("aria-label", `Message ${whom}`);
    drawSuggestion();
    const here = R.threadHref(S.sel);
    $("hint").textContent = S.pending.some((p) => p.where === here && p.state === "new") ? "Sending…" : sess && sess.state === "asleep" ? "Asleep. A message wakes it." : "Lands as the next turn if Claude is busy";
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
    const info = cmdInfo();
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
    // With nothing to offer (no list yet, or nothing matches what is typed)
    // the menu stays shut: a note there would only cover the line being
    // typed, and a command not listed is still sent as typed.
    const list = S.menu || typing ? commandsFor(typing ? input.value : "") : [];
    show("commands", list.length > 0);
    if (!list.length) return;
    box.textContent = "";
    for (const c of list) {
      const row = View.el("button", "cmd");
      row.type = "button";
      row.append(View.el("span", "cmd-name", "/" + c.name + (c.hint ? " " + c.hint : "")), View.el("span", "cmd-desc", c.description));
      row.onclick = () => { input.value = "/" + c.name + " "; S.menu = false; input.focus(); drawCommands(); };
      box.appendChild(row);
    }
  }

  // makeSession makes what a draft stands for: a repo's own lead, or a
  // thread, whose lead may stand up helpers and whose work lands through
  // git. In a folder known not to be a git repo there is nothing to land,
  // so a thread there is a plain session, as it always was.
  async function makeSession(ws, lead) {
    const plain = { cwd: ws.repoPath, workspaceId: ws.id, model: S.draftModel, advisor: S.draftAdvisor };
    if (lead) return call("POST", "/v1/sessions", { ...plain, kind: "lead" });
    if (ws.git && ws.git.isGitRepo === false) return call("POST", "/v1/sessions", plain);
    return (await call("POST", "/v1/threads", { workspaceId: ws.id, model: S.draftModel, advisor: S.draftAdvisor })).session;
  }

  // ensureSession makes the lead or thread a draft stands for (a lead that
  // is there already is answered with, never made twice). Every message
  // still on its way from that draft goes to the new session, and the page
  // moves there if it still shows the draft.
  async function ensureSession(item) {
    if (item.sel.kind === "session") return item.sel.id;
    const ws = S.workspaces.find((w) => w.id === item.sel.ws);
    if (!ws) throw new Error("this repo is no longer listed");
    const sess = await makeSession(ws, item.sel.as === "lead");
    S.sessions = S.sessions.filter((s) => s.id !== sess.id).concat(sess);
    const to = { kind: "session", id: sess.id };
    const shown = R.threadHref(S.sel) === item.where;
    Pending.rehome(S.pending, item.where, R.threadHref(to), to);
    if (shown) select(to);
    return sess.id;
  }

  async function uploadImages(id, images) {
    const names = [];
    for (const img of images) {
      const blob = await Img.prepare(img.file);
      names.push((await call("POST", `/v1/sessions/${id}/images`, blob)).name);
    }
    return names;
  }

  // earlierBlocks is what goes above the session's conversation: the ones
  // its clean starts left, each read once.
  function earlierBlocks(toBlocks) {
    const sess = S.sel.kind === "session" ? session() : null;
    if (!sess || !(sess.leftConversations || []).length) return [];
    Earlier.load(sess);
    return EarlierView.blocks(sess.leftConversations, Earlier.turnsFor(sess.id), toBlocks, Date.now());
  }

  // coldNow is what to ask before a message to the session on screen, when
  // its prompt cache has gone cold (threadgauge.js coldAsk); null when
  // nothing. An agent starts fresh by its own base's rule: never asked.
  function coldNow() {
    const sess = S.sel.kind === "session" ? session() : null;
    return sess && sess.kind !== "agent" ? Gauge.coldAsk(S.stream.result, Date.now(), S.stream.busy) : null;
  }

  // send sends the composer's message; on a cold cache it asks first
  // whether the message starts a clean conversation (threadclean.js), and
  // Cancel sends nothing.
  function send() {
    const input = $("input");
    if (!input.value.trim() && !S.images.length) return;
    const cold = coldNow();
    if (!cold) { sendNow(""); return; }
    Clean.ask(Gauge.coldText(cold)).then((pick) => {
      if (pick === null) { input.focus(); return; }
      sendNow(pick === "clean" ? "c" + Date.now().toString(36) + Math.random().toString(36).slice(2, 8) : "");
    });
  }

  // sendNow shows the message in the conversation at once, empties the
  // composer and leaves the sending to pump. clean is its clean-start key
  // ("" for none); a clean start keeps what is on screen of the
  // conversation it leaves (leaving), which stays above.
  function sendNow(clean) {
    const input = $("input");
    const text = input.value.trim();
    if (!text && !S.images.length) return;
    const sess = session();
    const leaving = clean && sess ? { conv: sess.conversation || sess.id, turns: S.stream.turns } : null;
    const images = S.images;
    S.pending.push({ key: ++S.pendingKey, sel: S.sel, where: R.threadHref(S.sel), state: "new", id: "", error: "", text, urls: images.map((img) => img.url), images, clean, leaving });
    input.value = "";
    Mention.close();
    Object.assign(S, { images: [], menu: false, error: "", said: "", jump: true });
    drawAll();
    pump();
  }

  // pump sends the messages one at a time, oldest first, so they land in
  // the order sent and a draft is made into a session once. One that
  // failed holds the rest of its conversation (threadpending.js).
  async function pump() {
    if (S.pumping) return;
    S.pumping = true;
    try {
      for (let item = Pending.next(S.pending); item; item = Pending.next(S.pending)) await deliver(item);
    } finally {
      S.pumping = false;
    }
  }

  // deliver makes the session if this is its first message, uploads the
  // pictures, then queues the prompt. The prompt's id is what its turn in
  // the stream will be called, which is when the page stops drawing it
  // itself. One that fails stays where it was sent, saying why, for Retry
  // or Remove.
  async function deliver(item) {
    try {
      const id = await ensureSession(item);
      const names = await uploadImages(id, item.images);
      const out = await call("POST", `/v1/sessions/${id}/prompt`, { text: item.text, sender: S.user, images: names, clean: item.clean || undefined });
      if (item.leaving) Earlier.seed(id, item.leaving.conv, item.leaving.turns);
      Object.assign(item, { state: "queued", id: out.id || "" });
    } catch (e) {
      Object.assign(item, { state: "failed", error: e.message });
    }
    schedule();
  }

  // retryPending sends a failed message again; dropPending removes it,
  // which lets the ones that waited behind it go.
  function retryPending(key) {
    const item = S.pending.find((p) => p.key === key);
    if (item) Object.assign(item, { state: "new", error: "" });
    schedule();
    pump();
  }

  function dropPending(key) {
    const item = S.pending.find((p) => p.key === key);
    if (item) item.urls.forEach((u) => URL.revokeObjectURL(u));
    S.pending = S.pending.filter((p) => p !== item);
    schedule();
    pump();
  }

  // --- the header's actions ---

  // act runs one session action and says why when it fails.
  async function act(work) {
    S.error = "";
    S.said = ""; // what the last action answered is about that action only
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

  function pickAdvisor(value) {
    if (!session()) { S.draftAdvisor = value; drawAll(); return; }
    saveAdvisor(value);
  }

  const saveAdvisor = (value) => act(async (sess) => {
    await call("POST", `/v1/sessions/${sess.id}/advisor`, { advisor: value });
    sess.advisor = value;
    S.later.delete(sess.id);
    S.restarted = false;
  });

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
    ["hello", (m) => { Tasks.refresh(); Todo.refresh(); return (m.attention || []).map(noteAttention).some(Boolean); }], // and the queue and to-do changes missed while away
    ["tasks-changed", () => { Tasks.refresh(); }],
    ["todos-changed", () => { Todo.refresh(); }],
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
    if (e.key === "Escape" && (Caps.escape() || NewRepo.escape() || Hosts.escape() || Settings.escape())) return;
    if (e.key === "Escape" && S.menu) { S.menu = false; drawCommands(); return; }
    if (e.metaKey || e.ctrlKey || e.altKey || editable(e.target)) return; // a digit typed into the composer is a digit
    if ("1234".includes(e.key) && View.pressKey($("cards"), e.key)) e.preventDefault();
  }

  // offered is the next message Claude Code predicts for the session on
  // screen (threadstream.js offer), "" when there is none to show now.
  const offered = () => (S.sel.kind === "session" && !Past.on() ? Stream.offer(S.stream, $("input").value) : "");

  // drawSuggestion shows the prediction as the empty box's grey text, with
  // a button for screens that have no Tab key.
  function drawSuggestion() {
    const text = offered();
    $("input").placeholder = text;
    show("suggest", !!text);
  }

  // useSuggestion puts the prediction in the box to send or change.
  function useSuggestion() {
    const text = offered();
    if (!text) return false;
    const input = $("input");
    input.value = text;
    input.focus();
    drawSuggestion();
    return true;
  }

  function onComposerKey(e) {
    if (Mention.key(e)) return; // the open "@" list takes its keys first
    if (e.key === "Tab" && !e.shiftKey && useSuggestion()) { e.preventDefault(); return; }
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
  }

  function wireComposer() {
    const input = $("input");
    $("composer").onsubmit = (e) => { e.preventDefault(); send(); };
    input.onkeydown = onComposerKey;
    input.oninput = () => { S.menu = false; drawCommands(); drawSuggestion(); Mention.changed(); };
    $("suggest").onclick = useSuggestion;
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
    $("advisor").onchange = (e) => pickAdvisor(e.target.value);
    $("restart-now").onclick = restartNow;
    $("restart-later").onclick = restartNow;
    $("loadout-restart").onclick = restartNow;
    $("later").onclick = () => { const sess = session(); if (sess) S.later.add(sess.id); drawAll(); };
    $("stop").onclick = () => act((sess) => call("POST", `/v1/sessions/${sess.id}/abort`));
    $("sleep").onclick = () => setWanted("asleep");
    $("end").onclick = () => { if (confirm("End this session? Its conversation stays on disk, but it will not run again.")) setWanted("done"); };
    $("rename").onclick = rename;
    $("merge-thread").onclick = () => threadAct("merge");
    $("end-thread").onclick = () => threadAct("end", undefined, "Close this thread? Its helpers are stood down. Work that is not merged stays on its branch.");
    $("stop-merge").onclick = () => threadAct("stop-merge", undefined, "Stop this merge? Its checks are ended. A repo that already landed stays landed.");
    $("past").onclick = (e) => Past.open(e);
    $("past-back").onclick = () => Past.back();
    $("new-thread").onclick = () => { const ws = workspace(); if (ws) select({ kind: "draft", ws: ws.id, as: "thread" }); };
    $("side-toggle").onclick = () => $("page").classList.toggle("side-open");
    $("add-repo").onclick = () => { // in the group of the repo on screen, if any
      const ws = workspace(), win = ws ? window.ccmuxThreadGroups.windowOf(S.windows, ws.id) : null;
      NewRepo.open(win ? win.name : "");
    };
    for (const tab of document.querySelectorAll(".tab")) {
      tab.onclick = () => {
        for (const t of document.querySelectorAll(".tab")) t.classList.toggle("on", t === tab);
        for (const name of ["changes", "team", "steps"]) show("tab-" + name, tab.dataset.tab === name);
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
    await Side.load(); // before the first draw, so the groups do not jump into place
    Tasks.refresh();
    Todo.refresh();
    connectFeed();
    if (S.sel.kind === "session") openSock();
    await loadLists();
    setInterval(loadLists, 20000);
    setInterval(() => drawGauge(session()), 15000);
    setInterval(drawMerge, 1000); // a running merge counts its seconds
  }

  boot();
})();
