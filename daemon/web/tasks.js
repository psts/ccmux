// tasks.js: the lens's task queue (daemon/internal/taskq). The view itself
// is taskview.js, shared with the thread page; this file hands it the lens
// and draws the queue's row in the sidebar. Each window's queue opens from
// a row under that window (placed like its Attention row, app.js) with the
// counts.
//
// It is a view that holds no session, like the attention board: a class on
// #app (style.css) puts the list where the terminal was.
// Runs after app.js, tasklist.js and taskview.js and uses their globals ($,
// esc, state, getUser, renderList, leaveWorkspace, openWorkspace,
// closeDrawer, ccmuxTaskList, ccmuxTaskView) at call time.
(() => {
  "use strict";
  const L = () => window.ccmuxTaskList;

  const view = window.ccmuxTaskView.create({
    user: () => getUser(),
    windows: () => state.windows || [],
    box: () => $("tasks"),
    show() {
      if (window.ccmuxBoard) window.ccmuxBoard.close();
      leaveWorkspace();
      $("app").classList.add("tasks-mode");
      $("empty").style.display = "none";
      closeDrawer();
    },
    hide: () => $("app").classList.remove("tasks-mode"),
    changed: () => renderList(),
    openSession: (wsId, paneId) => openWorkspace(wsId, paneId), // openWorkspace leaves the view
  });

  // row is window win's Tasks entry ({id, name}).
  function row(win) {
    const s = L().summary(view.counts(win.id));
    const li = document.createElement("li");
    li.className = "ws board-row tasks-row" + (view.openWindow() === win.id ? " active" : "") + (s.alert ? " waiting" : "");
    li.innerHTML = `<div class="ws-row"><span class="board-icon">☰</span><span class="name">Tasks</span>` +
      (s.text ? `<span class="tasks-sum">${esc(s.text)}</span>` : "") +
      (s.alert ? `<span class="board-count">${s.alert}</span>` : "") + `</div>`;
    li.onclick = () => view.open(win);
    return li;
  }

  window.ccmuxTasks = { refresh: view.refresh, row, open: (win) => view.open(win), close: view.close, isOpen: view.isOpen, openWindow: view.openWindow };
  view.refresh();
})();
