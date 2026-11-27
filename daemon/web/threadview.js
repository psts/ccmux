// threadview.js: the thread page's drawing. Each function takes plain data
// (a block from threadwork.js, a card from threadstream.js, the sidebar's
// tree from threadmodel.js) and returns elements; nothing here fetches or
// keeps state. Text always goes in as text, never as markup.
(() => {
  "use strict";
  const MD = window.ccmuxThreadMD;
  const Model = window.ccmuxThreadModel;
  const Tq = window.ccmuxTaskList;

  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  };
  const add = (parent, ...kids) => { for (const k of kids) if (k) parent.appendChild(k); return parent; };

  const mediaURL = (sessionId, name) => `/v1/sessions/${encodeURIComponent(sessionId)}/media/${encodeURIComponent(name)}`;

  // localThumb is a picture the browser still holds (a message on its way).
  function localThumb(url, caption) {
    const img = el("img");
    img.src = url;
    img.alt = caption;
    return add(el("figure", "thumb"), img, el("figcaption", "", caption));
  }

  // thumb is one stored image, opened full size by a click.
  function thumb(sessionId, file, caption) {
    const fig = el("figure", "thumb");
    const link = el("a");
    link.href = mediaURL(sessionId, file.name);
    link.target = "_blank";
    link.rel = "noopener";
    const img = el("img");
    img.src = link.href;
    img.alt = caption;
    img.loading = "lazy";
    add(link, img);
    return add(fig, link, el("figcaption", "", caption));
  }

  // diff draws one file's change: added lines green, removed red.
  function diff(d, path) {
    const box = el("div", "diff");
    add(box, el("div", "diff-path", path + (d.truncated ? " · the first lines of a longer change" : "")));
    const pre = el("pre");
    for (const h of d.hunks || []) {
      if (h.oldStart) add(pre, el("span", "hunk", `@@ -${h.oldStart} +${h.newStart} @@`)); // a place in the file, when the patch knows one
      for (const line of h.lines) add(pre, el("span", line[0] === "+" ? "add" : line[0] === "-" ? "del" : "", line || " "));
    }
    return add(box, pre);
  }

  function todoList(todos) {
    const box = el("div", "todos");
    for (const t of todos) add(box, add(el("div", "todo " + t.status), el("span", "box"), el("span", "todo-text", t.text)));
    return box;
  }

  // stepMore is what an opened step shows under its line: the diff it made,
  // the step list it set, the images it returned, its output or its error.
  function stepMore(s, ctx) {
    const st = s.part.state || {};
    const more = el("div", "step-more");
    if (st.diff) add(more, diff(st.diff, s.what));
    if (s.kind === "TodoWrite" && st.input && st.input.todos) add(more, todoList(st.input.todos.map((t) => ({ text: t.content, status: t.status }))));
    const pics = el("div", "thumbs");
    (st.images || []).forEach((f, i) => add(pics, thumb(ctx.sessionId, f, `Image ${i + 1} of ${st.images.length}`)));
    if (pics.childElementCount) add(more, pics);
    const text = st.error || (st.diff ? "" : st.output);
    if (text) add(more, el("pre", "out" + (st.error ? " error" : ""), String(text).slice(0, 20000)));
    return more.childElementCount ? more : null;
  }

  const chevron = '<svg class="chev" aria-hidden="true" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><polyline points="9 6 15 12 9 18"></polyline></svg>';

  // work is a run of tool calls: one line, opened to its steps by a click.
  function work(b, ctx) {
    const open = ctx.isOpen(b.id);
    const box = el("div", `b-work ${b.status}${open ? " open" : ""}`);
    const head = el("button", "work-head");
    head.type = "button";
    head.setAttribute("aria-expanded", String(open));
    head.innerHTML = chevron; // a fixed icon, no data in it
    add(head, el("span", "work-sum", b.summary), el("span", "work-out", b.status === "running" ? "working…" : b.outcome));
    head.onclick = () => ctx.toggle(b.id);
    add(box, head);
    if (!open) return box;
    const steps = el("div", "steps");
    for (const s of b.steps) {
      add(steps, add(el("div", "step " + s.status), el("span", "step-kind", s.kind), el("span", "step-what", s.what), el("span", "step-note", s.note)), stepMore(s, ctx));
    }
    return add(box, steps);
  }

  const pendingSays = { sending: "Sending…", queued: "Sent, waiting for Claude", held: "Waits for the message above" };

  // failedLine is what a message that could not be sent says, with what
  // can be done about it.
  function failedLine(b, ctx) {
    const line = el("div", "sending failed", "Not sent: " + b.error + " ");
    const button = (label, fn) => { const x = el("button", "link", label); x.type = "button"; x.onclick = () => fn(b.key); return x; };
    return add(line, button("Retry", ctx.retry), button("Remove", ctx.drop));
  }

  // user is the human's message. One still on its way (b.pending, drawn
  // by the page before the stream has it) shows its pictures from the
  // browser and says it is being sent.
  function user(b, ctx) {
    const box = el("div", "b-user" + (b.pending ? " pending" : ""));
    const pics = el("div", "thumbs");
    b.files.forEach((f, i) => add(pics, thumb(ctx.sessionId, f, `Pasted image · ${i + 1} of ${b.files.length}`)));
    (b.urls || []).forEach((u, i) => add(pics, localThumb(u, `Pasted image · ${i + 1} of ${b.urls.length}`)));
    if (pics.childElementCount) add(box, pics);
    if (b.text) add(box, el("div", "bubble", b.text));
    // Edit goes back to this message (threadedit.js): one that has landed,
    // in a conversation that is not an earlier one read from disk.
    if (!b.pending && !b.old && ctx.edit) add(box, editButton(b, ctx)); // an old conversation's message: Claude is no longer in it
    if (b.pending === "failed") add(box, failedLine(b, ctx));
    else if (b.pending) add(box, el("div", "sending", pendingSays[b.pending]));
    return box;
  }

  function editButton(b, ctx) {
    const x = el("button", "b-edit", "Edit");
    x.type = "button";
    x.title = "Go back to this message and edit it";
    x.onclick = () => ctx.edit(b);
    return x;
  }

  // whenSaid is when a message was: its clock time beside who said it, the
  // whole date and time on hover. A message with no time shows none.
  function whenSaid(at) {
    if (!at) return null;
    const d = new Date(at);
    const x = el("time", "said-at", d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }));
    x.dateTime = d.toISOString();
    x.title = d.toLocaleString([], { dateStyle: "full", timeStyle: "medium" });
    return x;
  }

  function claude(b, ctx) {
    const head = add(el("div", "who"), el("span", "", ctx.who), whenSaid(b.at));
    const said = add(el("div", "said"), head, MD.render(document, el("div", "md"), b.text));
    return add(el("div", "b-claude"), el("span", "avatar"), said);
  }

  // note is a line beside the conversation. One Claude Code wrote into the
  // conversation itself (a hook's feedback, a skill's text) can be long: it
  // folds to its first line.
  function note(b) {
    if (!b.own) return el("div", "b-note " + b.level, b.text);
    const box = el("details", "b-note");
    add(box, el("summary", "", "Claude Code: " + b.text.split("\n")[0].slice(0, 120)), el("div", "", b.text));
    return box;
  }

  const bus = (b) => add(el("div", "b-bus"), el("span", "dot"), add(el("span"), el("strong", "", b.from + " "), document.createTextNode(b.text)));

  // divider is the line where a clean start began a new conversation
  // (threadearlier.js).
  function divider(b) {
    const line = el("div", "b-divider", b.text);
    line.setAttribute("role", "separator");
    return line;
  }

  const draw = { user, claude, work, note, bus, divider, error: (b) => el("div", "b-error", b.text) };

  // block draws one block; sig is all of it that the drawing depends on, so
  // the page redraws a block only when its sig changed.
  const block = (b, ctx) => { const e = draw[b.kind](b, ctx); e.dataset.block = b.id; return e; };

  function sig(b, ctx) {
    if (b.kind !== "work") return JSON.stringify(b);
    const open = ctx.isOpen(b.id);
    const steps = open ? b.steps.map((s) => [s.status, s.note, s.what, ((s.part.state || {}).output || "").length]) : b.steps.length;
    return JSON.stringify([b.summary, b.outcome, b.status, open, steps]);
  }

  // --- cards: a question or an approval waiting on the human ---

  // keyed gives a card's first four choices the keys 1 to 4.
  function keyed(cardEl) {
    const asked = cardEl.querySelectorAll(".ac-q-opt");
    const buttons = [...(asked.length ? asked : cardEl.querySelectorAll(".ac-perm-btn"))].slice(0, 4);
    buttons.forEach((b, i) => { b.dataset.key = String(i + 1); b.prepend(el("span", "key", String(i + 1))); });
    return cardEl;
  }

  // tapAnswers makes a tap on an option the answer when the card asks one
  // question with one choice, as the mockup has it. The card is the agent
  // chat's, which keeps an Answer button whenever a free answer is allowed
  // (always, for Claude); here the typed answer and its button stay for
  // that, and a tap, or its key, is enough for the rest.
  function tapAnswers(cardEl, req, answer) {
    const qs = req.questions || [];
    if (qs.length !== 1 || qs[0].multiple) return;
    for (const b of cardEl.querySelectorAll(".ac-q-opt")) {
      const label = b.querySelector(".ac-q-label").textContent;
      b.onclick = () => answer({ t: "question", id: req.id, answers: [[label]] });
    }
  }

  // frameAnswer maps the card's answer frame (agentchat.js's, as the agent
  // chat sends it on its socket) onto the body of POST .../cards/{id}.
  function frameAnswer(frame) {
    if (frame.t === "permission") return { kind: "permission", reply: frame.reply };
    if (frame.t === "question-reject") return { kind: "question", reject: true };
    return { kind: "question", answers: frame.answers };
  }

  // card draws one waiting card. ctx.answer(card, body) sends the answer and
  // resolves to "" or to why it was not taken.
  function card(c, ctx) {
    const chat = window.ccmuxAgentChat;
    const box = el("section", "ask");
    box.dataset.card = c.id;
    box.setAttribute("aria-label", c.kind === "question" ? "Question from Claude" : "Claude asks for approval");
    add(box, add(el("div", "ask-head"), el("span", "ask-label", "Needs you"), el("span", "ask-when", "asked " + Model.ago(c.at, ctx.now))));
    const err = el("div", "ask-error hidden");
    const answer = async (frame) => {
      for (const b of box.querySelectorAll("button")) b.disabled = true;
      const why = await ctx.answer(c, frameAnswer(frame));
      if (!why) return;
      err.textContent = "Not sent: " + why;
      err.classList.remove("hidden");
      for (const b of box.querySelectorAll("button")) b.disabled = false;
    };
    const inner = c.kind === "question" ? chat.questionCard(c.req, answer) : chat.permissionCard(c.req, "Claude", answer);
    if (c.kind === "question") tapAnswers(inner, c.req, answer);
    add(box, keyed(inner));
    const change = c.kind === "permission" && ctx.diffFor(c.req);
    if (change) add(box, diff(change, ctx.rel(change.path)));
    return add(box, err);
  }

  // pressKey answers the first waiting card with its n-th choice; false when
  // there is no such choice.
  function pressKey(cards, n) {
    const b = cards.querySelector(`.ask [data-key="${n}"]`);
    if (!b || b.disabled) return false;
    b.click();
    return true;
  }

  // --- the sidebar ---

  // withMenu gives a sidebar item its right-click menu (ctx.menu), and a
  // "⋯" button that opens the same menu on a touch screen.
  function withMenu(target, kind, item, ctx) {
    target.oncontextmenu = (e) => { e.preventDefault(); ctx.menu(kind, item, e); };
    const more = el("button", "side-more", "⋯");
    more.type = "button";
    more.title = kind === "group" ? "Move, open alone, skills and plugins…" : "Skills and plugins…";
    more.setAttribute("aria-label", `More for ${item.name}`);
    more.onclick = (e) => ctx.menu(kind, item, e);
    return more;
  }

  // tasksRow is a group's Tasks entry, on one line: its queue's counts on
  // the right, the number that needs a person highlighted (ctx.tasks,
  // threadtasks.js). Its classes are
  // its own, not a repo row's, so nothing that walks the repos finds it.
  function tasksRow(g, ctx) {
    const T = ctx.tasks;
    const s = T.taskSum(g);
    const row = el("button", "side-tasks" + (T.tasksOn(g) ? " on" : ""));
    row.type = "button";
    row.title = "The group's task queue";
    row.onclick = () => T.openGroup(g);
    return add(row, el("span", "st-icon", "☰"), el("span", "st-name", "Tasks"), el("span", "st-sub", s.text || (s.alert ? "needs you" : "nothing queued")),
      s.alert ? el("span", "st-alert", String(s.alert)) : null);
  }

  // repoTaskRows are the unfinished tasks a repo has a part in, under its
  // row like its threads, one line each: a dot (amber when it waits on a
  // person), its title and its number. Its status is in the row's title.
  // One opens the group's queue at that task. list is the repo's tasks
  // less those its builder's session row already stands for (repoEl).
  function repoTaskRows(g, r, ctx, list) {
    const T = ctx.tasks;
    if (!list.length) return null;
    const on = T.taskOn(g, r);
    const box = el("div", "repo-tasks");
    for (const t of list) {
      const row = el("button", "side-task" + (on ? " on" : "") + (Tq.needsPerson(t) ? " alert" : ""));
      row.type = "button";
      row.title = Tq.statusWord(t.status) + ". Open it in the group's task queue";
      row.onclick = () => T.pickTask(g, r, t);
      add(box, add(row, el("span", "st-dot"), el("span", "st-name", t.title), el("span", "st-id", "#" + t.id)));
    }
    return box;
  }

  // repoTasksOf is a repo's unfinished tasks (ctx.tasks) split in two: the
  // ones a builder session listed under the repo already stands for (by
  // session id, Tq.builderTasks), and the rest, which keep their own rows.
  // A running task so shows once, as its builder's conversation.
  function repoTasksOf(g, r, ctx) {
    const all = g && ctx.tasks ? ctx.tasks.repoTasks(g, r) : [];
    const built = Tq.builderTasks(all, r.path, new Set(r.threads.map((t) => t.session.id)));
    const shown = new Set(built.values());
    return { built, rest: all.filter((t) => !shown.has(t)) };
  }

  // threadRow is one of a repo's sessions. task is the task it builds when
  // that task's own row is left out: one that needs a person then puts the
  // task row's amber on this row's dot, and its status in the title.
  function threadRow(t, ctx, task) {
    const ask = !!task && Tq.needsPerson(task);
    const tr = el("button", "row thread" + (ctx.isSession(t.session) ? " on" : "") + attClass(t.state) + (ask ? " task-alert" : ""));
    tr.type = "button";
    tr.title = ask ? Tq.statusWord(task.status) + ": task #" + task.id + " waits on you" : t.sub || Model.stateLabel(t.state).toLowerCase();
    tr.onclick = () => ctx.pickSession(t.session);
    return add(tr, el("span", "dot " + t.state), add(el("span", "row-body"), el("span", "row-name", t.title)));
  }

  // todoBadge is a repo's to-do list in the sidebar (ctx.todos,
  // threadtodoview.js): a pill on the repo's row, left of its branch, with
  // how many items wait for someone, coloured by the most urgent of them;
  // its title says what they are. It opens the list in the main area. A
  // repo with nothing on its list has none, except the repo the page is in,
  // so a person can always add the first item. It is the row's sibling in
  // the repo's grid, laid over the row (threads.css), not inside it: a
  // button in a button is not allowed, and its click would pick the repo.
  function todoBadge(r, ctx) {
    const D = ctx.todos;
    if (!D) return null;
    const s = D.sum(r);
    const on = D.on(r);
    if (!s.open && !s.planning && !s.tasked && !on && !ctx.inRepo(r)) return null;
    const b = el("button", "side-todo" + (on ? " on" : ""));
    b.type = "button";
    b.title = "To-do: " + (s.text || "nothing open");
    b.setAttribute("aria-label", `To-do list of ${r.name}: ${s.text || "nothing open"}`);
    b.onclick = () => D.open(r);
    return add(b, el("span", "sd-count" + (s.open ? " u-" + s.top : ""), String(s.open)));
  }

  // attClass makes a row pulse like the lens's session list: orange until it
  // is answered, green until someone looks. Other states are the dot alone.
  const attClass = (state) => (state === "waiting" || state === "done" ? " att-" + state : "");

  function repoEl(r, ctx, g) {
    const box = el("div", "repo");
    const row = el("button", "row" + (ctx.isLead(r) ? " on" : "") + attClass(r.state));
    row.type = "button";
    row.title = (r.path ? r.path + "\n" : "") + (r.lead ? "lead " + Model.stateLabel(r.state).toLowerCase() : "no lead yet");
    row.onclick = () => ctx.pickRepo(r);
    // Its git state is in the Git tab (threadcommits.js), not here.
    const top = add(el("span", "row-top cols"), el("span", "row-name", r.name));
    add(box, add(row, el("span", "dot " + r.state), add(el("span", "row-body"), top)), withMenu(row, "repo", r, ctx));
    const todo = todoBadge(r, ctx);
    if (todo) { box.classList.add("has-todo"); add(box, todo); }
    const tasks = repoTasksOf(g, r, ctx);
    for (const t of r.threads) add(box, threadRow(t, ctx, tasks.built.get(t.session.id)));
    return add(box, g ? repoTaskRows(g, r, ctx, tasks.rest) : null);
  }

  // groupHead is a group's name; a group that is a window (not "Other")
  // has the menu that sets its skills and plugins, and, from the person's
  // own sidebar (ctx.side, threadsidectl.js), a chevron that folds it, the
  // alert of what is inside while folded, and a drag handle.
  function groupHead(g, ctx) {
    const name = el("div", "group-name", g.name);
    if (!g.id) return name;
    const S = ctx.side;
    const headEl = el("div", "group-head");
    if (!S) return add(headEl, name, withMenu(headEl, "group", g, ctx));
    const fold = S.canFold(g) ? foldButton(g, S) : null;
    if (fold) { name.classList.add("foldable"); name.onclick = () => S.toggle(g); }
    const alert = S.folded(g) ? S.alert(g) : null;
    if (alert && alert.cls) headEl.classList.add("att-" + alert.cls);
    if (S.canDrag(g)) dragHandle(headEl, g, S);
    const count = alert && alert.count ? el("span", "group-count", String(alert.count)) : null;
    if (count) count.title = alert.text;
    const sum = S.folded(g) ? el("span", "group-sum", groupSum(g)) : null;
    return add(headEl, fold, name, sum, count, withMenu(headEl, "group", g, ctx));
  }

  // groupSum is what a folded group holds, beside its name: "8 repos ·
  // 6 agents". An open group shows them itself.
  function groupSum(g) {
    const n = (k, word) => k + " " + word + (k === 1 ? "" : "s");
    const agents = (g.agents || []).length;
    return n(g.repos.length, "repo") + (agents ? " · " + n(agents, "agent") : "");
  }

  function foldButton(g, S) {
    const shut = S.folded(g);
    const b = el("button", "group-fold");
    b.innerHTML = chevron; // a fixed icon, no data in it; threads.css turns it down while open
    b.type = "button";
    b.title = shut ? "Show this group's repos" : "Fold this group into its name (its sessions keep running)";
    b.setAttribute("aria-expanded", String(!shut));
    b.setAttribute("aria-label", (shut ? "Show " : "Fold ") + g.name);
    b.onclick = () => S.toggle(g);
    return b;
  }

  // dragHandle lets a mouse move the group by its header.
  function dragHandle(headEl, g, S) {
    headEl.draggable = true;
    headEl.title = "Drag to move this group";
    headEl.ondragstart = (e) => S.dragStart(g, e);
    headEl.ondragend = () => S.dragEnd();
  }

  // groupEl is one group: its header, then (unless folded) its body,
  // indented under the name: its Tasks row, its repos, and its agents under
  // their own heading. Any part of a window's group is a place to drop a
  // dragged one.
  function groupEl(g, ctx) {
    const S = ctx.side;
    const box = el("div", "group");
    if (g.id) box.dataset.gid = g.id;
    if (S && g.id) {
      box.ondragover = (e) => S.dragOver(g, e, box);
      box.ondrop = (e) => S.drop(e);
    }
    add(box, groupHead(g, ctx));
    if (S && S.folded(g)) { box.classList.add("folded"); return box; }
    const tasks = g.id && ctx.tasks ? tasksRow(g, ctx) : null;
    const agents = (g.agents || []).map((a) => agentEl(a, ctx, g));
    const body = add(el("div", "group-body"), tasks, ...g.repos.map((r) => repoEl(r, ctx, g)), agents.length ? agentsHead() : null, ...agents);
    return add(box, body);
  }

  // agentsHead is the "Agents" heading, drawn like the Tasks row (an icon
  // and the word) so the two read as the same kind of thing. It is not a
  // button: nothing opens from it.
  function agentsHead() {
    return add(el("div", "side-sub"), el("span", "st-icon", "⚙"), el("span", "st-name", "Agents"));
  }

  // agentEl is a role agent's row, under its group's "Agents" heading: its
  // dot and its name. An agent that runs as a
  // session opens its conversation here, and its row pulses like a thread's;
  // one still in a terminal opens in the terminal lens, where its chat is.
  function agentEl(a, ctx, g) {
    const here = !!a.session && ctx.isSession(a.session);
    const row = el("button", "row agent" + (here ? " on" : "") + (a.session ? attClass(a.state) : ""));
    row.type = "button";
    row.title = a.session ? `${Model.stateLabel(a.state)}. Opens its conversation.` : `${a.state === "live" ? "Running" : "Asleep"}. Opens in the terminal lens.`;
    row.onclick = () => (a.session ? ctx.pickSession(a.session) : ctx.openAgent(a, g));
    const top = add(el("span", "row-top"), el("span", "row-name", a.name));
    return add(row, el("span", "dot " + a.state), add(el("span", "row-body"), top));
  }

  function sidebar(tree, groups, ctx) {
    tree.textContent = "";
    const note = ctx.side ? ctx.side.note() : "";
    if (note) add(tree, el("div", "side-note", note));
    if (ctx.side && ctx.side.allHref) {
      const all = el("a", "side-alone", "Only this group. Show every group");
      all.href = ctx.side.allHref;
      add(tree, all);
    }
    for (const g of groups) add(tree, groupEl(g, ctx));
    if (!groups.length) add(tree, el("div", "quiet", "No repos yet. Open one in the terminal lens first."));
  }

  // --- the right-hand tabs ---

  // What the Helpers tab says of its team: a thread's lead stands its
  // helpers down; a main session's helpers end by themselves.
  const helpersSay = {
    thread: ["No helpers. A thread stands one up when it needs another repo.", "Helpers this thread stood up. The lead stands them down when it is done."],
    side: ["No helpers. This session gets one when it asks another repo something.",
      "Helpers this session asked. Each ends by itself an hour after its last answer, or sooner when this session starts a new conversation or files a task."],
  };

  // helpersTab lists a thread's team: each member with what it is doing and
  // where it works. A row opens that member's conversation; a helper's has
  // the button that stands it down. ctx.side is a main session's helpers.
  function helpersTab(box, rows, ctx) {
    box.textContent = "";
    const say = helpersSay[ctx.side ? "side" : "thread"];
    if (!rows.length) return add(box, el("div", "quiet", say[0]));
    add(box, el("div", "quiet", say[1]));
    for (const r of rows) {
      const row = el("div", "member" + (ctx.isPicked(r) ? " on" : ""));
      const open = el("button", "member-open");
      open.type = "button";
      open.title = r.bus ? "Open " + r.bus : "Open";
      open.onclick = () => ctx.pick(r);
      add(open, el("span", "dot " + r.state), add(el("span", "row-body"),
        add(el("span", "row-top"), el("span", "row-name", r.name), el("span", "role", r.role)),
        el("span", "member-doing", r.doing), el("span", "member-where", r.where)));
      add(row, open);
      if (r.role === "helper" && ctx.canStandDown) {
        const down = el("button", "plain small", "Stand down");
        down.type = "button";
        down.onclick = () => ctx.standDown(r);
        add(row, down);
      }
      add(box, row);
    }
    return box;
  }

  // stepsTab is the session's own step list (Claude's TodoWrite), and what
  // its last turn cost. The repo's to-do list is another thing
  // (threadtodoview.js): what sessions noticed and left, kept by the daemon.
  function stepsTab(box, todos, result) {
    box.textContent = "";
    add(box, todos.length ? todoList(todos) : el("div", "quiet", "No step list in this session."));
    // A result can be the context report alone (session.context before any
    // bill), so the line needs both numbers.
    if (result && typeof result.durationMs === "number" && typeof result.costUSD === "number") {
      add(box, el("div", "cost", `Last turn ${(result.durationMs / 1000).toFixed(1)} s · session so far $${result.costUSD.toFixed(2)}`));
    }
  }

  window.ccmuxThreadView = { el, block, sig, card, pressKey, frameAnswer, sidebar, helpersTab, stepsTab, diff, mediaURL };
})();
