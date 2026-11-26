// threadtodoview.js: a repo's to-do list on the thread page. It reads every
// repo's list from the daemon (GET /v1/todos), answers what the sidebar
// draws under each repo (threadview.js: how many items wait, and how urgent
// the worst is), and shows one repo's list in the main area, in place of
// the conversation, where a person adds items, re-ranks them, ticks them
// off, deletes them and makes one a task. Make task puts the item in the
// repo's planning line: the daemon sends it to the repo's planner, a
// session of its own that plans it and files it, and never to the repo's
// main session. How the list reads is threadtodo.js; the rules are the
// daemon's (internal/todos).
//
// create() takes the page:
//   user()      the name the page signs with
//   model()     the model a repo's planner is started on when it has none yet
//   sessions()  the sessions the page knows, for the link to a repo's planner
//   sessionHref(id)  the address of a session's conversation
//   openSession(id)  show that conversation here
//   call(method, path, body)   the page's daemon call; it throws with the reason
//   leave()     take the conversation (and the task queue) off screen
//   changed()   the counts or the open list changed: redraw the page
//   confirm(q)  ask before something that cannot be undone
(() => {
  "use strict";
  const T = window.ccmuxThreadTodo;
  const ago = (since, now) => window.ccmuxThreadModel.ago(since, now);

  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  };
  const add = (parent, ...kids) => { for (const k of kids) if (k) parent.appendChild(k); return parent; };
  const button = (cls, text, onclick) => Object.assign(el("button", cls, text), { type: "button", onclick });

  function urgencyPicker(value, title) {
    const pick = el("select", "rt-urgency u-" + value);
    pick.title = title;
    for (const u of T.URGENCIES) add(pick, Object.assign(el("option", "", u), { value: u, selected: u === value }));
    pick.value = value;
    return pick;
  }

  function create(page) {
    // ws is the repo session whose list is on show ("" while it is closed);
    // repos maps each repo session to its list's key (the daemon's answer).
    const q = { todos: [], repos: {}, ws: "", name: "", path: "", doneOpen: false, said: "", bad: false };
    const box = () => document.getElementById("repo-todo");
    const keyOf = (r) => q.repos[r.id] || r.path;
    const listOf = (r) => T.ofRepo(q.todos, keyOf(r));
    const shown = () => T.ofRepo(q.todos, q.repos[q.ws] || q.path);

    // --- the daemon ---

    async function refresh() {
      try {
        const d = await page.call("GET", "/v1/todos");
        q.todos = d.todos || [];
        q.repos = d.repos || {};
      } catch (e) {
        console.warn("to-do: lists unreadable:", e.message);
        if (q.ws) say("Couldn't read the to-do list: " + e.message, true);
        return;
      }
      page.changed();
      if (q.ws) drawList();
    }

    // send makes one change and reads the lists back. A refusal is shown,
    // not thrown: it is the daemon saying no to a person.
    async function send(method, path, body, said) {
      try {
        await page.call(method, path, body);
      } catch (e) {
        say(e.message, true);
        return false;
      }
      say(said || "", false);
      await refresh();
      return true;
    }

    function say(text, bad) {
      q.said = text;
      q.bad = !!bad;
      const line = box().querySelector(".rt-status");
      if (!line) return;
      line.textContent = text;
      line.classList.toggle("bad", !!bad);
    }

    // --- open / close ---

    // open shows repo row r's list (threadmodel.js's repoRow).
    function open(r) {
      const was = q.ws;
      Object.assign(q, { ws: r.id, name: r.name, path: r.path, said: "", bad: false });
      if (!was) page.leave();
      document.getElementById("page").classList.add("todo-mode");
      box().classList.remove("hidden");
      build();
      page.changed();
      refresh();
    }

    function close() {
      if (!q.ws) return;
      q.ws = "";
      document.getElementById("page").classList.remove("todo-mode");
      box().classList.add("hidden");
      box().textContent = "";
      page.changed();
    }

    // --- drawing ---

    // build draws the frame once per open: the line a person adds an item
    // on, and the box the list is redrawn into. A list that changes under
    // someone (an agent adds an item) never touches what they are typing.
    function build() {
      const b = box();
      b.textContent = "";
      const text = Object.assign(el("input", "rt-text"), { type: "text", maxLength: 600, placeholder: "Add an item: what is wrong or left to do, and where" });
      text.setAttribute("aria-label", "New to-do item");
      const urgency = urgencyPicker("medium", "How urgent the new item is");
      const form = add(el("form", "rt-add"), text, urgency, Object.assign(el("button", "primary small", "Add"), { type: "submit" }));
      form.onsubmit = async (e) => {
        e.preventDefault();
        if (!text.value.trim()) return;
        if (await send("POST", "/v1/todos", { repo: q.path, text: text.value, urgency: urgency.value, by: page.user() })) text.value = "";
      };
      add(b, add(el("div", "rt-head"), el("span", "rt-status" + (q.bad ? " bad" : ""), q.said)), form, el("div", "rt-list"));
      drawList();
    }

    function drawList() {
      const list = box().querySelector(".rt-list");
      if (!list) return;
      const { open: unfinished, done } = T.split(shown());
      const now = Date.now();
      list.textContent = "";
      if (!unfinished.length) add(list, el("p", "rt-empty", `Nothing on ${q.name}'s to-do list. Its sessions add what they notice and leave; you can add items above.`));
      for (const t of unfinished) add(list, itemEl(t, now));
      if (done.length) add(list, doneSection(done, now));
    }

    function itemEl(t, now) {
      const row = el("div", `rt-item u-${t.urgency} s-${t.status}`);
      row.dataset.id = String(t.id);
      const urgency = urgencyPicker(t.urgency, "Urgency");
      urgency.onchange = () => send("PATCH", `/v1/todos/${t.id}`, { urgency: urgency.value });
      const meta = add(el("div", "rt-meta"), el("span", "rt-id", "#" + t.id), el("span", "", T.byline(t, now, ago)), chipEl(t),
        t.note ? el("span", "rt-note", t.note) : null);
      return add(row, urgency, add(el("div", "rt-body"), el("div", "rt-item-text", t.text), meta), actionsEl(t));
    }

    // chipEl is where an item someone has taken is. One in the planning
    // line links to the planner's conversation, when the repo has a planner
    // now (one that ended is started again by the daemon within a minute).
    function chipEl(t) {
      const chip = T.chip(t);
      if (!chip) return null;
      const planner = t.status === "planning" ? T.plannerOf(page.sessions(), q.repos, t.repo) : null;
      if (!planner) return el("span", "rt-chip", chip);
      const link = Object.assign(el("a", "rt-chip rt-chip-link", chip), { href: page.sessionHref(planner.id), title: `Open ${q.name}'s planner` });
      link.onclick = (e) => { e.preventDefault(); page.openSession(planner.id); };
      return link;
    }

    function doneSection(done, now) {
      const fold = el("details", "rt-done");
      fold.open = q.doneOpen;
      fold.ontoggle = () => { q.doneOpen = fold.open; };
      add(fold, el("summary", "", `Done (${done.length})`));
      for (const t of done) {
        const row = el("div", "rt-item s-done");
        row.dataset.id = String(t.id);
        add(fold, add(row, add(el("div", "rt-body"), el("div", "rt-item-text", t.text),
          add(el("div", "rt-meta"), el("span", "rt-id", "#" + t.id), el("span", "", T.doneLine(t, now, ago)))), actionsEl(t)));
      }
      return fold;
    }

    function actionsEl(t) {
      const box = el("div", "rt-actions");
      for (const a of T.actions(t)) {
        const b = button("plain small rt-act-" + a, T.label(a), () => acts[a](t));
        b.dataset.act = a;
        add(box, b);
      }
      return box;
    }

    // --- acting ---

    // makeTask puts the item in the repo's planning line: the daemon sends
    // it to the repo's planner, which plans it and files it, and starts
    // that planner when the repo has none. sendAgain is the same call for
    // an item that is in the line already.
    const toPlanner = (t, said) => send("POST", `/v1/todos/${t.id}/task`, { workspaceId: q.ws, sender: page.user(), model: page.model() }, said);
    const makeTask = (t) => toPlanner(t, `Added #${t.id} to ${q.name}'s planning line.`);
    const sendAgain = (t) => toPlanner(t, `Sent #${t.id} to ${q.name}'s planner again.`);

    // What each button does, by its action (threadtodo.js's actions).
    const acts = {
      task: makeTask,
      again: sendAgain,
      done: (t) => send("PATCH", `/v1/todos/${t.id}`, { status: "done", by: page.user() }),
      reopen: (t) => send("PATCH", `/v1/todos/${t.id}`, { status: "open" }),
      delete: (t) => { if (page.confirm(`Delete to-do #${t.id}? It is gone for good.`)) send("DELETE", `/v1/todos/${t.id}`); },
    };

    // sidebar is what threadview.js's sidebar asks of the lists.
    const sidebar = {
      sum: (r) => T.summary(listOf(r)),
      on: (r) => q.ws === r.id,
      open,
    };

    // title is the header's line while a list is on show.
    const title = () => ({ title: "To-do", meta: `${q.name} · what its sessions noticed and left, and what you add` });

    // sessionsChanged hears that the page read its sessions again: a list
    // on show is redrawn, since an item in the planning line links to the
    // repo's planner, which may have just been made (the first Make task)
    // or replaced (the daemon started a new one for a line left behind).
    const sessionsChanged = () => { if (q.ws) drawList(); };

    return { refresh, open, close, isOpen: () => !!q.ws, title, sidebar, sessionsChanged };
  }

  window.ccmuxThreadTodoView = { create };
})();
