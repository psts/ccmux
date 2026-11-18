// threadtasks.js: the task queue on the thread page. The view is the lens's
// own (taskview.js over tasklist.js); this file hands it the thread page and
// answers what the sidebar draws (threadview.js): each group's Tasks row,
// and under each repo the unfinished tasks it has a part in, whoever filed
// them. A row opens the group's queue in the main area, in place of the
// conversation; picking anything else on the page closes it.
(() => {
  "use strict";
  const L = window.ccmuxTaskList;

  // create takes the page: user() and windows() as the lens's are, leave()
  // takes the conversation off screen, changed() redraws the page, and
  // openSession(wsId, paneId) shows a worker's terminal (the lens has it).
  function create(page) {
    const box = () => document.getElementById("tasks");
    const view = window.ccmuxTaskView.create({
      user: page.user, windows: page.windows, box, openSession: page.openSession, changed: page.changed,
      show() { page.leave(); document.getElementById("page").classList.add("tasks-mode"); },
      hide() { document.getElementById("page").classList.remove("tasks-mode"); },
    });
    const winOf = (id) => (page.windows() || []).find((w) => w.id === id) || { id, name: "" };
    // repoName is a repo's name as a task's parts say it (the daemon's
    // filepath.Base of the part's repo path), for the queue's repo filter.
    const repoName = (r) => String(r.path || "").split("/").filter(Boolean).pop() || r.name;

    // sidebar is what threadview.js's sidebar asks of the queue.
    const sidebar = {
      taskSum: (g) => L.summary(view.counts(g.id)),
      tasksOn: (g) => view.isOpen() && view.openWindow() === g.id && !view.openRepo(),
      openGroup: (g) => view.open(winOf(g.id), { repo: "" }),
      repoTasks: (g, r) => (g.id && r.path ? L.repoTasks(view.tasks(), g.id, r.path) : []),
      taskOn: (g, r) => view.isOpen() && view.openWindow() === g.id && view.openRepo() === repoName(r),
      pickTask: (g, r, t) => view.open(winOf(g.id), { repo: repoName(r), task: t.id }),
    };

    // title is the header's line while the queue is on show.
    function title() {
      const win = winOf(view.openWindow());
      return { title: "Tasks", meta: (win.name ? win.name + " · " : "") + "the group's task queue: what its repos' Claudes filed, and how it runs" };
    }

    return { refresh: view.refresh, close: view.close, isOpen: view.isOpen, title, sidebar };
  }

  window.ccmuxThreadTasks = { create };
})();
