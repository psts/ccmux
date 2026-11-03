// tasks.js: the task queue (daemon/internal/taskq). A repo's session files a
// plan with the peers `tasks` tool; this page shows every repo's tasks from a
// sidebar row with the counts, lets a person fix a plan before it runs, put
// it in order, mark it ready, cancel it and clear it away. How the list reads
// is tasklist.js; what a task may do next arrives with each task from the
// daemon, so a button here is one the daemon accepts.
//
// It is a view that holds no session, like the attention board: a class on
// #app (style.css) puts the list where the terminal was.
// Runs after app.js and tasklist.js and uses their globals ($, esc, state,
// getUser, renderList, leaveWorkspace, openWorkspace, closeDrawer,
// ccmuxTaskList) at call time.
(() => {
  "use strict";
  const L = () => window.ccmuxTaskList;
  const q = { tasks: [], counts: {}, open: false, repo: "", expanded: new Set(), drafts: new Map(), pending: false };
  const url = (p) => `/v1/tasks${p || ""}?user=${encodeURIComponent(getUser())}`;
  const box = () => $("tasks");

  // --- the daemon ---

  async function refresh() {
    let body;
    try {
      const r = await fetch(url(), { cache: "no-store" });
      if (!r.ok) throw new Error(`${r.status} ${await r.text()}`);
      body = await r.json();
    } catch (e) {
      console.warn("tasks: list unreadable:", e);
      if (q.open) status("Couldn't read the task list: " + e.message, true);
      return;
    }
    q.tasks = body.tasks || [];
    q.counts = body.counts || {};
    renderList();
    if (q.open) render();
  }

  // send makes one change and reads the list back; a refusal is shown, not
  // thrown, since it is the daemon saying no to a person.
  async function send(path, method, body) {
    try {
      const r = await fetch(url(path), {
        method, headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined,
      });
      if (!r.ok) {
        const t = await r.text();
        let msg = t;
        try { msg = JSON.parse(t).error || t; } catch (_) { /* plain text */ }
        status(msg, true);
        return false;
      }
    } catch (e) {
      status("Couldn't reach the daemon: " + e.message, true);
      return false;
    }
    await refresh();
    return true;
  }

  // --- the sidebar row ---

  function row() {
    const s = L().summary(q.counts);
    const li = document.createElement("li");
    li.className = "ws board-row tasks-row" + (q.open ? " active" : "") + (s.alert ? " waiting" : "");
    li.innerHTML = `<div class="ws-row"><span class="board-icon">☰</span><span class="name">Tasks</span>` +
      (s.text ? `<span class="tasks-sum">${esc(s.text)}</span>` : "") +
      (s.alert ? `<span class="board-count">${s.alert}</span>` : "") + `</div>`;
    li.onclick = open;
    return li;
  }

  // --- open / close ---

  function open() {
    if (q.open) return;
    if (window.ccmuxBoard) window.ccmuxBoard.close();
    leaveWorkspace();
    q.open = true;
    $("app").classList.add("tasks-mode");
    $("empty").style.display = "none";
    box().classList.remove("hidden");
    closeDrawer();
    render();
    renderList();
    refresh();
  }

  function close() {
    if (!q.open) return;
    q.open = false;
    $("app").classList.remove("tasks-mode");
    box().classList.add("hidden");
    box().innerHTML = "";
    renderList();
  }

  // --- drawing ---

  function status(text, bad) {
    const s = box().querySelector(".tq-status");
    if (!s) return;
    s.textContent = text;
    s.classList.toggle("bad", !!bad);
  }

  // render redraws the list, except while someone is typing in it: then it
  // waits for the box to lose focus, so a change elsewhere never eats typing.
  function render() {
    const b = box();
    if (b.contains(document.activeElement) && /^(TEXTAREA|INPUT)$/.test(document.activeElement.tagName)) {
      q.pending = true;
      return;
    }
    q.pending = false;
    wire(b);
    const repos = L().repos(q.tasks);
    if (q.repo && !repos.includes(q.repo)) q.repo = "";
    const secs = L().sections(q.tasks, q.repo);
    b.innerHTML = head(repos) + (secs.length ? secs.map(sectionHtml).join("") : emptyHtml());
  }

  function head(repos) {
    const opts = [`<option value="">All repos</option>`]
      .concat(repos.map((r) => `<option value="${esc(r)}"${r === q.repo ? " selected" : ""}>${esc(r)}</option>`));
    return `<div class="tq-head"><span class="tq-title">Tasks</span>` +
      `<select class="tq-repo" data-act="repo">${opts.join("")}</select>` +
      `<span class="tq-status"></span></div>`;
  }

  function emptyHtml() {
    return `<p class="tq-empty">No tasks yet. Plan a feature with a repo's Claude, then ask it to file the plan: ` +
      `it lands here and waits for a free slot.</p>`;
  }

  function sectionHtml(sec) {
    return `<section class="tq-sec tq-${sec.key}"><h3>${esc(sec.title)} <span class="tq-n">${sec.tasks.length}</span></h3>` +
      sec.tasks.map(taskHtml).join("") + `</section>`;
  }

  function taskHtml(t) {
    const w = L().waitsOn(t, q.tasks);
    const open = q.expanded.has(t.id);
    return `<div class="tq-task${open ? " open" : ""}" data-id="${t.id}">` +
      `<div class="tq-line" data-act="toggle"><span class="tq-id">#${t.id}</span>` +
      `<span class="tq-repo-chip">${esc(t.repo)}</span><span class="tq-name">${esc(t.title)}</span>` +
      (w ? `<span class="tq-after${w.blocked ? " blocked" : ""}">${esc(w.text)}</span>` : "") +
      (t.note ? `<span class="tq-note">${esc(t.note)}</span>` : "") + `</div>` +
      (open ? detailHtml(t) : "") + `</div>`;
  }

  function detailHtml(t) {
    const d = q.drafts.get(t.id) || { title: t.title, plan: t.plan };
    const text = t.editable
      ? `<input class="tq-edit-title" data-field="title" maxlength="120" value="${esc(d.title)}">` +
        `<textarea class="tq-edit-plan" data-field="plan" spellcheck="false">${esc(d.plan)}</textarea>`
      : `<pre class="tq-plan">${esc(t.plan)}</pre>`;
    const where = t.branch ? ` · branch ${esc(t.branch)}` : "";
    return `<div class="tq-detail">${text}<div class="tq-meta">filed by ${esc(t.filedBy || "?")}${where}</div>` +
      `<div class="tq-actions">${buttonsHtml(t)}</div></div>`;
  }

  function buttonsHtml(t) {
    const b = (act, label, extra) => `<button type="button" data-act="${act}"${extra || ""}>${esc(label)}</button>`;
    const out = [];
    if (t.editable) out.push(b("save", "Save"), b("up", "↑"), b("down", "↓"));
    if (t.afterId && t.orderable) out.push(b("unwait", `Stop waiting on #${t.afterId}`));
    if (t.workspaceId) out.push(b("session", "Open session"));
    for (const to of t.moves || []) out.push(b("move", L().moveLabel(to), ` data-to="${to}"`));
    if (t.removable) out.push(b("remove", "Remove"));
    return out.join("");
  }

  // --- acting ---

  // wire hangs the handlers on the box once; every redraw reuses them.
  function wire(b) {
    if (b.dataset.wired) return;
    b.dataset.wired = "1";
    b.addEventListener("click", onClick);
    b.addEventListener("input", onInput);
    b.addEventListener("change", (e) => {
      if (e.target.dataset.act === "repo") { q.repo = e.target.value; render(); }
    });
    b.addEventListener("focusout", () => setTimeout(() => { if (q.pending) render(); }, 0));
  }

  function onInput(e) {
    const field = e.target.dataset.field, el = e.target.closest(".tq-task");
    if (!field || !el) return;
    const id = Number(el.dataset.id), t = q.tasks.find((x) => x.id === id);
    if (!t) return;
    const d = q.drafts.get(id) || { title: t.title, plan: t.plan };
    d[field] = e.target.value;
    q.drafts.set(id, d);
  }

  function onClick(e) {
    const btn = e.target.closest("[data-act]"), el = e.target.closest(".tq-task");
    if (!btn || !el || btn.dataset.act === "repo") return;
    act(btn.dataset.act, Number(el.dataset.id), btn.dataset.to);
  }

  function toggle(id) {
    if (q.expanded.has(id)) q.expanded.delete(id); else q.expanded.add(id);
    render();
  }

  async function save(id) {
    const d = q.drafts.get(id);
    if (d && await send(`/${id}`, "PATCH", { title: d.title, plan: d.plan })) {
      q.drafts.delete(id);
      status("Saved #" + id);
    }
  }

  async function remove(id) {
    if (await send(`/${id}`, "DELETE")) q.expanded.delete(id);
  }

  // openSession shows the worker's pane: openWorkspace leaves this view.
  function openSession(id) {
    const t = q.tasks.find((x) => x.id === id);
    if (t && t.workspaceId) openWorkspace(t.workspaceId, t.paneId);
  }

  // What each button does, by its data-act.
  const actions = {
    toggle,
    save,
    remove,
    up: (id) => send(`/${id}/move`, "POST", { dir: "up" }),
    down: (id) => send(`/${id}/move`, "POST", { dir: "down" }),
    move: (id, to) => send(`/${id}`, "PATCH", { status: to }),
    unwait: (id) => send(`/${id}`, "PATCH", { afterId: 0 }),
    session: openSession,
  };

  function act(kind, id, to) {
    if (Object.hasOwn(actions, kind)) actions[kind](id, to);
  }

  window.ccmuxTasks = { refresh, row, open, close, isOpen: () => q.open };
  refresh();
})();
