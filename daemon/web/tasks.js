// tasks.js: the task queue (daemon/internal/taskq). Each ccmux window has its
// own queue: a repo's session files a plan into its window's queue with the
// peers `tasks` tool. Each window's queue opens from a row under that window
// (placed like its Attention row, app.js) with the counts, and lets a person
// fix a plan before it runs, put it in order, mark it ready, cancel it and
// clear it away. How the list reads
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
  // win is the window whose queue the view shows ("" while it is closed);
  // counts are keyed by window id.
  const q = { tasks: [], counts: {}, win: "", winName: "", repo: "", expanded: new Set(), drafts: new Map(), pending: false };
  const shown = () => q.tasks.filter((t) => t.windowId === q.win);
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
      if (q.win) status("Couldn't read the task list: " + e.message, true);
      return;
    }
    q.tasks = body.tasks || [];
    q.counts = body.counts || {};
    renderList();
    if (q.win) render();
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

  // row is window win's Tasks entry ({id, name}).
  function row(win) {
    const s = L().summary(q.counts[win.id] || {});
    const li = document.createElement("li");
    li.className = "ws board-row tasks-row" + (q.win === win.id ? " active" : "") + (s.alert ? " waiting" : "");
    li.innerHTML = `<div class="ws-row"><span class="board-icon">☰</span><span class="name">Tasks</span>` +
      (s.text ? `<span class="tasks-sum">${esc(s.text)}</span>` : "") +
      (s.alert ? `<span class="board-count">${s.alert}</span>` : "") + `</div>`;
    li.onclick = () => open(win);
    return li;
  }

  // --- open / close ---

  function open(win) {
    if (q.win === win.id) return;
    const wasOpen = !!q.win;
    q.win = win.id;
    q.winName = win.name || "";
    q.repo = "";
    if (wasOpen) { render(); renderList(); return; } // another window's queue, same view
    if (window.ccmuxBoard) window.ccmuxBoard.close();
    leaveWorkspace();
    $("app").classList.add("tasks-mode");
    $("empty").style.display = "none";
    box().classList.remove("hidden");
    closeDrawer();
    render();
    renderList();
    refresh();
  }

  function close() {
    if (!q.win) return;
    q.win = "";
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
    const mine = shown();
    const repos = L().repos(mine);
    if (q.repo && !repos.includes(q.repo)) q.repo = "";
    const secs = L().sections(mine, q.repo);
    b.innerHTML = head(repos) + (secs.length ? secs.map(sectionHtml).join("") : emptyHtml());
  }

  function head(repos) {
    const opts = [`<option value="">All repos</option>`]
      .concat(repos.map((r) => `<option value="${esc(r)}"${r === q.repo ? " selected" : ""}>${esc(r)}</option>`));
    const title = "Tasks" + (q.winName ? " · " + q.winName : "");
    return `<div class="tq-head"><span class="tq-title">${esc(title)}</span>` +
      `<select class="tq-repo" data-act="repo">${opts.join("")}</select>` +
      `<span class="tq-status"></span></div>`;
  }

  function emptyHtml() {
    return `<p class="tq-empty">No tasks in this window yet. Plan a feature with one of its repos' Claudes, then ask it to file the plan: ` +
      `it lands here and waits for a free slot.</p>`;
  }

  function sectionHtml(sec) {
    return `<section class="tq-sec tq-${sec.key}"><h3>${esc(sec.title)} <span class="tq-n">${sec.tasks.length}</span></h3>` +
      sec.tasks.map(taskHtml).join("") + `</section>`;
  }

  function taskHtml(t) {
    const w = L().waitsOn(t, shown());
    const open = q.expanded.has(t.id);
    const chips = (t.parts || []).map((p) =>
      `<span class="tq-repo-chip tq-part-${esc(p.status)}" title="${esc(L().partWord(p.status))}">${esc(p.repo)}</span>`).join("");
    return `<div class="tq-task${open ? " open" : ""}" data-id="${t.id}">` +
      `<div class="tq-line" data-act="toggle"><span class="tq-id">#${t.id}</span>` +
      `<span class="tq-name">${esc(t.title)}</span>${chips}` +
      (w ? `<span class="tq-after${w.blocked ? " blocked" : ""}">${esc(w.text)}</span>` : "") +
      (t.note ? `<span class="tq-note">${esc(t.note.split("\n")[0])}</span>` : "") + `</div>` +
      (open ? detailHtml(t) : "") + `</div>`;
  }

  // draft is what is typed and not saved yet: a task's title ("t" + id) or a
  // part's plan ("p" + id), falling back to the saved text.
  const draft = (key, saved) => (q.drafts.has(key) ? q.drafts.get(key) : saved);

  function detailHtml(t) {
    const title = t.editable
      ? `<input class="tq-edit-title" data-field="title" data-key="t${t.id}" maxlength="120" value="${esc(draft("t" + t.id, t.title))}">`
      : "";
    // A note of several lines (a failed merge's check output) shows in full here.
    const note = t.note && t.note.includes("\n") ? `<pre class="tq-plan tq-full-note">${esc(t.note)}</pre>` : "";
    return `<div class="tq-detail">${title}${note}${(t.parts || []).map((p) => partHtml(t, p)).join("")}` +
      `<div class="tq-meta">filed by ${esc(t.filedBy || "?")}</div>` +
      `<div class="tq-actions">${buttonsHtml(t)}</div></div>`;
  }

  function partHtml(t, p) {
    const plan = t.editable
      ? `<textarea class="tq-edit-plan" data-field="plan" data-key="p${p.id}" spellcheck="false">${esc(draft("p" + p.id, p.plan))}</textarea>`
      : `<pre class="tq-plan">${esc(p.plan)}</pre>`;
    const where = p.branch ? ` · ${esc(p.busName)} on ${esc(p.branch)}` : "";
    const b = (act, label) => `<button type="button" data-act="${act}">${esc(label)}</button>`;
    const buttons = (p.workspaceId ? b("session", "Open session") : "") + (p.retryable ? b("retry", "Retry") : "") +
      (p.sendBackable ? b("sendback", "Send back to worker") : "");
    return `<div class="tq-part" data-part="${p.id}"><div class="tq-part-head">` +
      `<span class="tq-repo-chip tq-part-${esc(p.status)}">${esc(p.repo)}</span>` +
      `<span class="tq-part-status">${esc(L().partWord(p.status))}</span>` +
      (p.note ? `<span class="tq-note">${esc(p.note)}</span>` : "") +
      `<span class="tq-meta">${where}</span>${buttons ? `<span class="tq-actions">${buttons}</span>` : ""}</div>${plan}</div>`;
  }

  function buttonsHtml(t) {
    const b = (act, label, extra) => `<button type="button" data-act="${act}"${extra || ""}>${esc(label)}</button>`;
    const out = [];
    if (t.editable) out.push(b("save", "Save"), b("up", "↑"), b("down", "↓"));
    if (t.afterId && t.orderable) out.push(b("unwait", `Stop waiting on #${t.afterId}`));
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
    const key = e.target.dataset.key;
    if (key) q.drafts.set(key, e.target.value);
  }

  function onClick(e) {
    const btn = e.target.closest("[data-act]"), el = e.target.closest(".tq-task");
    if (!btn || !el || btn.dataset.act === "repo") return;
    const part = btn.closest("[data-part]");
    act(btn.dataset.act, Number(el.dataset.id), btn.dataset.to, part ? Number(part.dataset.part) : 0);
  }

  function toggle(id) {
    if (q.expanded.has(id)) q.expanded.delete(id); else q.expanded.add(id);
    render();
  }

  // save sends the task's typed title and every typed plan of its parts.
  async function save(id) {
    const t = q.tasks.find((x) => x.id === id);
    if (!t) return;
    const sends = [];
    if (q.drafts.has("t" + id)) sends.push(["t" + id, `/${id}`, { title: q.drafts.get("t" + id) }]);
    for (const p of t.parts || []) {
      if (q.drafts.has("p" + p.id)) sends.push(["p" + p.id, `/${id}/parts/${p.id}`, { plan: q.drafts.get("p" + p.id) }]);
    }
    for (const [key, path, body] of sends) {
      if (!await send(path, "PATCH", body)) return; // the refusal is on screen; the rest stays typed
      q.drafts.delete(key);
    }
    status(sends.length ? "Saved #" + id : "Nothing to save");
  }

  async function remove(id) {
    if (await send(`/${id}`, "DELETE")) q.expanded.delete(id);
  }

  // openSession shows a part's worker pane: openWorkspace leaves this view.
  function openSession(id, _to, partId) {
    const t = q.tasks.find((x) => x.id === id);
    const p = t && (t.parts || []).find((x) => x.id === partId);
    if (p && p.workspaceId) openWorkspace(p.workspaceId, p.paneId);
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
    retry: (id, _to, partId) => send(`/${id}/parts/${partId}`, "PATCH", { retry: true }),
    // The worker gets the task's failure note and the part runs again.
    sendback: (id, _to, partId) => send(`/${id}/parts/${partId}/send-back`, "POST"),
  };

  function act(kind, id, to, partId) {
    if (Object.hasOwn(actions, kind)) actions[kind](id, to, partId);
  }

  window.ccmuxTasks = { refresh, row, open, close, isOpen: () => !!q.win, openWindow: () => q.win };
  refresh();
})();
