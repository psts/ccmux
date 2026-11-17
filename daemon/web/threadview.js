// threadview.js: the thread page's drawing. Each function takes plain data
// (a block from threadwork.js, a card from threadstream.js, the sidebar's
// tree from threadmodel.js) and returns elements; nothing here fetches or
// keeps state. Text always goes in as text, never as markup.
(() => {
  "use strict";
  const MD = window.ccmuxThreadMD;
  const Model = window.ccmuxThreadModel;

  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  };
  const add = (parent, ...kids) => { for (const k of kids) if (k) parent.appendChild(k); return parent; };

  const mediaURL = (sessionId, name) => `/v1/sessions/${encodeURIComponent(sessionId)}/media/${encodeURIComponent(name)}`;

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
  // the to-do list it set, the images it returned, its output or its error.
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

  function user(b, ctx) {
    const box = el("div", "b-user");
    const pics = el("div", "thumbs");
    b.files.forEach((f, i) => add(pics, thumb(ctx.sessionId, f, `Pasted image · ${i + 1} of ${b.files.length}`)));
    if (pics.childElementCount) add(box, pics);
    if (b.text) add(box, el("div", "bubble", b.text));
    return box;
  }

  function claude(b, ctx) {
    const said = add(el("div", "said"), el("span", "who", ctx.who), MD.render(document, el("div", "md"), b.text));
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

  const draw = { user, claude, work, note, bus, error: (b) => el("div", "b-error", b.text) };

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

  function gitBadges(git) {
    if (!git) return [];
    return [el("span", "branch", git.branch), git.dirty ? el("span", "dirty", "●" + git.dirty) : null, git.origin ? el("span", "ab", git.origin) : null];
  }

  function repoEl(r, ctx) {
    const box = el("div", "repo");
    const on = ctx.isLead(r);
    const row = el("button", "row" + (on ? " on" : ""));
    row.type = "button";
    row.onclick = () => ctx.pickRepo(r);
    const top = add(el("span", "row-top"), el("span", "row-name", r.name), ...gitBadges(r.git));
    const sub = (r.git && r.git.vs) || (r.lead ? "lead " + Model.stateLabel(r.state).toLowerCase() : "no lead yet");
    add(box, add(row, el("span", "dot " + r.state), add(el("span", "row-body"), top, el("span", "row-sub", sub))));
    if (r.git && ctx.inRepo(r)) add(box, add(el("div", "git-detail"), el("span", "", r.git.track), r.git.vs ? el("span", "", r.git.vs) : null, el("span", "quiet", r.git.clean)));
    for (const t of r.threads) {
      const tr = el("button", "row thread" + (ctx.isSession(t.session) ? " on" : ""));
      tr.type = "button";
      tr.onclick = () => ctx.pickSession(t.session);
      add(box, add(tr, el("span", "dot " + t.state), add(el("span", "row-body"), el("span", "row-name", t.title), el("span", "row-sub", t.sub || Model.stateLabel(t.state).toLowerCase()))));
    }
    return box;
  }

  function sidebar(tree, groups, ctx) {
    tree.textContent = "";
    for (const g of groups) add(tree, add(el("div", "group"), el("div", "group-name", g.name), ...g.repos.map((r) => repoEl(r, ctx))));
    if (!groups.length) add(tree, el("div", "quiet", "No repos yet. Open one in the terminal lens first."));
  }

  // --- the right-hand tabs ---

  // repoChanges is one repo of a thread in the Changes tab: whose branch it
  // is, the one line on what the branch holds, and its files.
  function repoChanges(g) {
    const box = el("div", "chg");
    add(box, add(el("div", "chg-head"), el("span", "chg-repo", g.repo), el("span", "branch", g.branch)));
    for (const f of g.files) add(box, add(el("div", "file still"), el("span", "file-path", f.path), el("span", "plus", f.added ? "+" + f.added : ""), el("span", "minus", f.removed ? "−" + f.removed : "")));
    return add(box, el("div", "chg-status", g.status));
  }

  // changesTab draws a thread's changes by repo (groups, from the daemon's
  // read of each member's branch), then what this session itself changed
  // with its diffs (files, from its conversation).
  function changesTab(box, files, picked, onPick, groups = []) {
    box.textContent = "";
    for (const g of groups) add(box, repoChanges(g));
    if (groups.length && files.length) add(box, el("div", "chg-own", "Edits in this conversation"));
    if (!files.length) return groups.length ? box : add(box, el("div", "quiet", "No files changed in this session yet."));
    for (const f of files) {
      const row = el("button", "file" + (picked === f.path ? " on" : ""));
      row.type = "button";
      row.onclick = () => onPick(f.path);
      add(box, add(row, el("span", "file-path", f.path), el("span", "plus", f.added ? "+" + f.added : ""), el("span", "minus", f.removed ? "−" + f.removed : "")));
    }
    const shown = files.find((f) => f.path === picked) || files[files.length - 1];
    return add(box, diff(shown.diff, shown.path));
  }

  // helpersTab lists a thread's team: each member with what it is doing and
  // where it works. A row opens that member's conversation; a helper's has
  // the button that stands it down.
  function helpersTab(box, rows, ctx) {
    box.textContent = "";
    if (!rows.length) return add(box, el("div", "quiet", "No helpers. A thread stands one up when it needs another repo."));
    add(box, el("div", "quiet", "Helpers this thread stood up. The lead stands them down when it is done."));
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

  function todoTab(box, todos, result) {
    box.textContent = "";
    add(box, todos.length ? todoList(todos) : el("div", "quiet", "No to-do list in this session."));
    if (result) add(box, el("div", "cost", `Last turn ${(result.durationMs / 1000).toFixed(1)} s · session so far $${result.costUSD.toFixed(2)}`));
  }

  window.ccmuxThreadView = { el, block, sig, card, pressKey, frameAnswer, sidebar, changesTab, helpersTab, todoTab, diff, mediaURL };
})();
