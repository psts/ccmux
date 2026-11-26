// threadtodo.js: how a repo's to-do list reads on the thread page, as plain
// functions over the daemon's GET /v1/todos answer, so threadtodo_test.js
// can drive them without a browser. threadtodoview.js owns the DOM and the
// daemon calls.
//
// A repo's to-do list is what its sessions noticed and did not handle right
// away (they add to it themselves, with the peers `todo` tool), plus what a
// person adds. It is not a session's own step list (Claude's TodoWrite),
// which is the Steps tab. The rules (what is the same item, what a status
// change does) live in daemon/internal/todos and are not copied here.
(function (root) {
  "use strict";

  // The urgencies, most urgent first, as the picker lists them.
  const URGENCIES = ["high", "medium", "low"];
  const RANK = { high: 3, medium: 2, low: 1 };
  const rank = (t) => RANK[t.urgency] || 0;

  // ofRepo is the items on the list whose key is key (the repo's main
  // folder, as the daemon names it).
  function ofRepo(todos, key) {
    return (todos || []).filter((t) => t.repo === key);
  }

  // split is a list as the page shows it: what is not done, high urgency
  // before medium before low and the newest first within each; then the
  // done ones, the latest done first.
  function split(list) {
    const open = (list || []).filter((t) => t.status !== "done")
      .sort((a, b) => rank(b) - rank(a) || b.createdAt - a.createdAt || b.id - a.id);
    const done = (list || []).filter((t) => t.status === "done").sort((a, b) => (b.doneAt || 0) - (a.doneAt || 0));
    return { open, done };
  }

  // summary is a repo's row in the sidebar: how many items wait for someone
  // (open), the highest urgency among those, how many more are in the
  // repo's planning line (planning: sent to its planner, not filed yet) or
  // already with a task (tasked), and the line under the name.
  function summary(list) {
    const waiting = (list || []).filter((t) => t.status === "open");
    const count = (status) => (list || []).filter((t) => t.status === status).length;
    const planning = count("planning"), tasked = count("tasked");
    const top = waiting.reduce((best, t) => (rank(t) > (RANK[best] || 0) ? t.urgency : best), "");
    const parts = URGENCIES.map((u) => [waiting.filter((t) => t.urgency === u).length, u]).filter(([n]) => n).map(([n, u]) => `${n} ${u}`);
    if (planning) parts.push(`${planning} planning`);
    if (tasked) parts.push(`${tasked} tasked`);
    return { open: waiting.length, planning, tasked, top, text: parts.join(" · ") };
  }

  // chip says where an item someone has taken is: in the repo's planning
  // line (Make task sent it to the repo's planner, which has not filed it
  // yet), or with a task. An item sent to the repo's own session before
  // there were planners is tasked with no task. "" for an item nobody has
  // taken.
  function chip(t) {
    if (t.status === "planning") return "planning";
    if (t.status !== "tasked") return "";
    return t.taskId ? `task #${t.taskId}` : "sent to the repo's session to file";
  }

  // plannerOf is the planner session of the repo whose list is key: the
  // session of kind planner that has not ended, in one of the repo's
  // sessions (repos maps a repo session to its list's key). null when the
  // repo has none: its line then waits for the daemon to start one.
  function plannerOf(sessions, repos, key) {
    return (sessions || []).find((s) => s.kind === "planner" && s.state !== "done" && (repos || {})[s.workspaceId] === key) || null;
  }

  // actions are the buttons an item gets. An open item can be made a task;
  // one in the planning line can be sent to the planner again or put back
  // on the list; so can one sent to a session that filed nothing (it goes
  // into the line); one a task has is that task's, so it only ticks off or
  // goes.
  function actions(t) {
    if (t.status === "done") return ["reopen", "delete"];
    if (t.status === "open") return ["task", "done", "delete"];
    if (t.status === "planning") return ["again", "reopen", "done", "delete"];
    return t.taskId ? ["done", "delete"] : ["again", "done", "delete"];
  }

  const LABELS = { task: "Make task", again: "Send again", done: "Done", delete: "Delete", reopen: "Put back" };
  const label = (action) => LABELS[action] || action;

  // byline is who added an item and when; doneLine who finished it, when,
  // and their note. ago turns a time into "4 min ago" (threadmodel.js).
  function byline(t, now, ago) {
    return `added by ${t.addedBy || "someone"} · ${ago(t.createdAt, now)}`;
  }

  function doneLine(t, now, ago) {
    return `done by ${t.doneBy || "someone"} · ${ago(t.doneAt, now)}` + (t.note ? ` · ${t.note}` : "");
  }

  const api = { URGENCIES, ofRepo, split, summary, chip, plannerOf, actions, label, byline, doneLine };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadTodo = api;
})(typeof window !== "undefined" ? window : this);
