// tasklist.js: how the task queue reads on screen, as plain functions over
// the daemon's GET /v1/tasks answer, so tasklist_test.js can drive them
// without a browser. tasks.js owns the DOM and the daemon calls. What a task
// may do next (its moves, editable, removable) comes from the daemon with
// each task: the rules live in daemon/internal/taskq and are not copied here.
(function (root) {
  "use strict";

  // The lists in the order the page shows them: what needs a person first,
  // then what is moving, then the queue, then history.
  const SECTIONS = [
    ["needs_you", "Needs you"],
    ["ready_to_merge", "Ready to merge"],
    ["running", "Running"],
    ["waiting", "Waiting"],
    ["planning", "Planning"],
    ["failed", "Failed"],
    ["done", "Done"],
    ["cancelled", "Cancelled"],
  ];

  // The words a move button carries, by the status it moves to.
  const MOVE_LABELS = { waiting: "Ready to run", planning: "Back to planning", cancelled: "Cancel" };

  // sections splits the tasks by status in queue order, keeping one repo's
  // when repo is set, and drops the empty lists.
  function sections(tasks, repo) {
    const out = [];
    for (const [key, title] of SECTIONS) {
      const list = tasks.filter((t) => t.status === key && (!repo || t.repo === repo));
      if (list.length) out.push({ key, title, tasks: list });
    }
    return out;
  }

  // repos lists the repos that have tasks, by name.
  function repos(tasks) {
    return [...new Set(tasks.map((t) => t.repo))].sort();
  }

  // summary is the sidebar row's line: what waits, runs and finished, and
  // how many need a person (the row's highlighted count).
  function summary(counts) {
    const c = counts || {};
    const parts = [];
    if (c.running) parts.push(`${c.running} running`);
    if (c.waiting) parts.push(`${c.waiting} waiting`);
    if (c.done) parts.push(`${c.done} done`);
    return { text: parts.join(" · "), alert: (c.needs_you || 0) + (c.ready_to_merge || 0) };
  }

  // waitsOn says what a task waits for: nothing, a task still to finish, or
  // one that failed or was cancelled, which holds it until someone acts.
  function waitsOn(task, tasks) {
    if (!task.afterId) return null;
    const dep = tasks.find((t) => t.id === task.afterId);
    if (!dep) return { text: `after #${task.afterId} (removed)`, blocked: true };
    const label = `#${dep.id} ${dep.repo}`;
    if (dep.status === "done") return { text: `after ${label} (done)`, blocked: false };
    if (dep.status === "failed" || dep.status === "cancelled") {
      return { text: `blocked: ${label} is ${dep.status}`, blocked: true };
    }
    return { text: `after ${label}`, blocked: false };
  }

  function moveLabel(to) { return MOVE_LABELS[to] || to; }

  const api = { sections, repos, summary, waitsOn, moveLabel, SECTIONS };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxTaskList = api;
})(typeof window !== "undefined" ? window : this);
