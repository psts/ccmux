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

  // builtWith says what a part's Claude is started with, from the model and
  // effort the daemon saves on the part (taskq.Run): "" for a part from
  // before parts had them.
  function builtWith(part) {
    if (!part.model) return "";
    return part.effort ? `${part.model} · ${part.effort} effort` : part.model;
  }

  // summary is the sidebar row's line: what runs, merges, waits and
  // finished, and how many need a person (the row's highlighted count). A
  // built feature merges on its own, so it is no person's job, but it keeps
  // its window's slot until it is merged: without "merging" a row would read
  // "2 waiting" with nothing at work while the checks run.
  function summary(counts) {
    const c = counts || {};
    const parts = [];
    const merging = (c.ready_to_merge || 0) + (c.merging || 0);
    if (c.running) parts.push(`${c.running} running`);
    if (merging) parts.push(`${merging} merging`);
    if (c.waiting) parts.push(`${c.waiting} waiting`);
    if (c.done) parts.push(`${c.done} done`);
    return { text: parts.join(" · "), alert: (c.needs_you || 0) + (c.merge_failed || 0) };
  }

  // waitsOn says what a task waits for: nothing, the tasks it starts after
  // (all of them have to finish), or the ones among them that were
  // cancelled or removed, which hold it until someone acts.
  function waitsOn(task, tasks) {
    const deps = (task.afterIds || []).map((id) => depOf(id, tasks));
    if (!deps.length) return null;
    const dead = deps.filter((d) => d.blocked);
    if (dead.length) return { text: "blocked: " + dead.map((d) => d.text).join(", "), blocked: true };
    return { text: "after " + deps.map((d) => d.text).join(", "), blocked: false };
  }

  // depOf is one task another starts after, as its label names it.
  function depOf(id, tasks) {
    const dep = tasks.find((t) => t.id === id);
    if (!dep) return { text: `#${id} (removed)`, blocked: true };
    const label = `#${dep.id} ${dep.title}`;
    if (dep.status === "cancelled") return { text: `${label} is cancelled`, blocked: true };
    return { text: dep.status === "done" ? `${label} (done)` : label, blocked: false };
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

  // priorityChoices is the Tasks header's priority picker beside the slots
  // one, for a window whose slots are slots and priority is prio: none (0,
  // served after every numbered window) and 1 to MAX_PRIORITY, the daemon's
  // own bound (manager.MaxTaskPriority). No slots number means no task
  // runner, and so no picker either.
  const MAX_PRIORITY = 9;
  function priorityChoices(slots, prio) {
    if (typeof slots !== "number") return [];
    const now = typeof prio === "number" ? prio : 0;
    const out = [{ value: 0, label: "No priority", selected: now === 0 }];
    for (let v = 1; v <= MAX_PRIORITY; v++) out.push({ value: v, label: `Priority ${v}`, selected: v === now });
    return out;
  }

  // ORDER is each status's place in the page's sections; FINISHED is what
  // a repo's row leaves out.
  const ORDER = new Map(SECTIONS.map(([key], i) => [key, i]));
  const FINISHED = new Set(["done", "cancelled"]);

  // repoTasks is what a repo's row on the thread page lists: the unfinished
  // tasks of window windowId with a part in the repo at repoPath, whoever
  // filed them, in the order the queue's sections read (what needs a
  // person first). A task with parts in two repos is under both.
  function repoTasks(tasks, windowId, repoPath) {
    const mine = (tasks || []).filter((t) => t.windowId === windowId && !FINISHED.has(t.status) &&
      partsOf(t).some((p) => p.repoPath === repoPath));
    return mine.sort((a, b) => (ORDER.get(a.status) ?? 99) - (ORDER.get(b.status) ?? 99));
  }

  // statusWord is a task's status as its section's title says it.
  const TITLES = new Map(SECTIONS);
  function statusWord(status) { return TITLES.get(status) || status; }

  // needsPerson is whether a task waits on a person (the row's highlight).
  function needsPerson(t) { return t.status === "needs_you" || t.status === "merge_failed"; }

  const api = {
    sections, repos, summary, waitsOn, moveLabel, partWord, builtWith, slotChoices, priorityChoices,
    repoTasks, statusWord, needsPerson, SECTIONS,
  };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxTaskList = api;
})(typeof window !== "undefined" ? window : this);
