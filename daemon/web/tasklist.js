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
    ["merge_failed", "Merge failed"],
    ["ready_to_merge", "Ready to merge"],
    ["merging", "Merging"],
    ["running", "Running"],
    ["waiting", "Waiting"],
    ["planning", "Planning"],
    ["done", "Done"],
    ["cancelled", "Cancelled"],
  ];

  // The words a move button carries, by the status it moves to.
  const MOVE_LABELS = {
    waiting: "Ready to run", planning: "Back to planning", cancelled: "Cancel",
    done: "Mark merged", ready_to_merge: "Try merge again",
  };

  const partsOf = (t) => t.parts || [];

  // sections splits the tasks by status, keeping those with a part in repo
  // when repo is set, and drops the empty lists. Done reads newest first and
  // Waiting next-to-start first (SORTS); the rest keep queue order.
  function sections(tasks, repo) {
    const out = [];
    for (const [key, title] of SECTIONS) {
      const list = tasks.filter((t) => t.status === key && (!repo || partsOf(t).some((p) => p.repo === repo)));
      if (SORTS[key]) SORTS[key](list);
      if (list.length) out.push({ key, title, tasks: list });
    }
    return out;
  }

  // place is a waiting task's place in line from the daemon's ahead (the
  // unfinished tasks it waits on, taskq.Ahead): -1, held until a person
  // acts, goes last. The daemon's ↑/↓ trade within one place in line, so
  // they move the row next to it in this order.
  const place = (t) => (t.ahead >= 0 ? t.ahead : Infinity);

  // Array.prototype.sort is stable, so ties keep queue order.
  const SORTS = {
    done: (list) => list.sort((a, b) => (b.finishedAt || 0) - (a.finishedAt || 0)),
    waiting: (list) => list.sort((a, b) => (place(a) === place(b) ? 0 : place(a) - place(b))),
  };

  // repos lists the repos that have a part in any task, by name.
  function repos(tasks) {
    return [...new Set(tasks.flatMap((t) => partsOf(t).map((p) => p.repo)))].sort();
  }

  // PART_WORDS are a part's statuses as the page says them.
  const PART_WORDS = { pending: "not started", running: "working", needs_you: "needs you", ready: "done", failed: "failed" };
  function partWord(status) { return PART_WORDS[status] || status; }

  // summary is the sidebar row's line: what waits, runs and finished, and
  // how many need a person (the row's highlighted count). A built feature
  // merges on its own now, so ready_to_merge is no longer a person's job.
  function summary(counts) {
    const c = counts || {};
    const parts = [];
    if (c.running) parts.push(`${c.running} running`);
    if (c.waiting) parts.push(`${c.waiting} waiting`);
    if (c.done) parts.push(`${c.done} done`);
    return { text: parts.join(" · "), alert: (c.needs_you || 0) + (c.merge_failed || 0) };
  }

  // waitsOn says what a task waits for: nothing, a task still to finish, or
  // one that was cancelled, which holds it until someone acts.
  function waitsOn(task, tasks) {
    if (!task.afterId) return null;
    const dep = tasks.find((t) => t.id === task.afterId);
    if (!dep) return { text: `after #${task.afterId} (removed)`, blocked: true };
    const label = `#${dep.id} ${dep.title}`;
    if (dep.status === "done") return { text: `after ${label} (done)`, blocked: false };
    if (dep.status === "cancelled") {
      return { text: `blocked: ${label} is ${dep.status}`, blocked: true };
    }
    return { text: `after ${label}`, blocked: false };
  }

  function moveLabel(to) { return MOVE_LABELS[to] || to; }

  // slotChoices is the Tasks header's "how many at once" picker for a window
  // whose number is n: Paused (0) to MAX_SLOTS, the daemon's own bound
  // (manager.MaxTaskSlots). No number (no task runner on the daemon) means
  // no picker.
  const MAX_SLOTS = 3;
  function slotChoices(n) {
    if (typeof n !== "number") return [];
    const out = [];
    for (let v = 0; v <= MAX_SLOTS; v++) {
      out.push({ value: v, label: v === 0 ? "Paused" : `${v} at a time`, selected: v === n });
    }
    return out;
  }

  const api = { sections, repos, summary, waitsOn, moveLabel, partWord, slotChoices, SECTIONS };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxTaskList = api;
})(typeof window !== "undefined" ? window : this);
