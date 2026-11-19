// taskview.js: the task queue's view (daemon/internal/taskq), shared by
// both pages: the lens (tasks.js) and the thread page (threadtasks.js).
// Each ccmux window has its own queue: a repo's session files a plan into
// its window's queue with the peers `tasks` tool. The view shows one
// window's queue and lets a person fix a plan before it runs, put it in
// order, mark it ready, cancel it and clear it away, and set how many of the
// window's tasks build at once and its priority. How the list reads is
// tasklist.js; what a task may do next arrives with each task from the
// daemon, so a button here is one the daemon accepts.
//
// It reads nothing of the page it is on: the page hands create() a host,
//   user()           the name the daemon reads the list for
//   windows()        the window list (GET /v1/windows): taskSlots, taskPriority
//   box()            the element the list draws into
//   show() / hide()  put the view on screen and take it off
//   changed()        the counts or the open window changed: redraw the sidebar
//   openSession(ws, pane, session)  show a part's worker: its builder's
//                    conversation when session names one, else the worker's
//                    terminal pane in its repo's tmux session (a worker from
//                    before builders were sessions)
// and uses only dom.js's esc, tasklist.js (ccmuxTaskList) and threadmd.js
// (ccmuxThreadMD, to read a builder's report) besides.
(() => {
  "use strict";
  const L = () => window.ccmuxTaskList;
  const MD = () => window.ccmuxThreadMD;

  function create(host) {
    // win is the window whose queue the view shows ("" while it is closed);
    // counts are keyed by window id.
    const q = { tasks: [], counts: {}, win: "", winName: "", repo: "", expanded: new Set(), reports: new Set(), drafts: new Map(), pending: false };
    const shown = () => q.tasks.filter((t) => t.windowId === q.win);
    const url = (p) => `/v1/tasks${p || ""}?user=${encodeURIComponent(host.user())}`;
    const box = () => host.box();
    const winOf = (id) => (host.windows() || []).find((w) => w.id === id);

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
      host.changed();
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

    // --- open / close ---

    // open shows window win's queue ({id, name}). opts.repo narrows it to
    // one repo's tasks and opts.task opens one task's details; without
    // opts, opening the window already on show changes nothing.
    function open(win, opts) {
      if (opts) {
        q.repo = opts.repo || "";
        if (opts.task) q.expanded.add(opts.task);
      }
      if (q.win === win.id) { if (opts) { render(); host.changed(); } return; }
      const wasOpen = !!q.win;
      q.win = win.id;
      q.winName = win.name || "";
      if (!opts) q.repo = "";
      if (wasOpen) { render(); host.changed(); return; } // another window's queue, same view
      host.show();
      box().classList.remove("hidden");
      render();
      host.changed();
      refresh();
    }

    function close() {
      if (!q.win) return;
      q.win = "";
      host.hide();
      box().classList.add("hidden");
      box().innerHTML = "";
      host.changed();
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
        `<select class="tq-repo" data-act="repo">${opts.join("")}</select>` + limitsHtml() +
        `<span class="tq-status"></span></div>`;
    }

    // limitsHtml is the window's two pickers, read from the window list the
    // page keeps: how many of its tasks build at once, and its priority.
    function limitsHtml() {
      const win = winOf(q.win);
      const pick = (cls, act, title, choices) => !choices.length ? "" :
        `<select class="tq-repo ${cls}" data-act="${act}" title="${esc(title)}">` +
        choices.map((c) => `<option value="${c.value}"${c.selected ? " selected" : ""}>${esc(c.label)}</option>`).join("") + `</select>`;
      return pick("tq-slots", "slots", "How many of this window's tasks build at once", L().slotChoices(win && win.taskSlots)) +
        pick("tq-prio", "priority", "Which window's tasks start first when the cap across windows (Settings › Tasks) is reached: 1 first, none last",
          L().priorityChoices(win && win.taskSlots, win && win.taskPriority));
    }

    // NUMBERS are the window's two numbers: where each is stored, its field
    // on the window list, and what the header says once it is saved.
    const NUMBERS = {
      slots: {
        path: "slots", field: "taskSlots", fail: "Couldn't change how many run at once: ",
        said: (n) => (n === 0 ? "Paused: nothing new starts in this window" : `Up to ${n} at a time`),
      },
      priority: {
        path: "priority", field: "taskPriority", fail: "Couldn't change the priority: ",
        said: (n) => (n === 0 ? "No priority: starts after every window that has one" : `Priority ${n}: starts before higher numbers and windows with none`),
      },
    };

    // setNumber stores one of the window's numbers; the runner uses it on its
    // next pass. The local window list takes it at once, so the picker does
    // not flick back before the page next reads the list.
    async function setNumber(kind, n) {
      const id = q.win, k = NUMBERS[kind];
      try {
        const r = await fetch(`/v1/windows/${encodeURIComponent(id)}/${k.path}`, {
          method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ [kind]: n }),
        });
        if (!r.ok) throw new Error(`${r.status} ${await r.text()}`);
      } catch (e) {
        render(); // the picker goes back to the stored number, then says why
        status(k.fail + e.message, true);
        return;
      }
      const win = winOf(id);
      if (win) win[k.field] = kind === "priority" && n === 0 ? undefined : n;
      render();
      status(k.said(n));
    }

    function emptyHtml() {
      const where = q.repo ? `No tasks for ${esc(q.repo)} in this window yet. ` : "No tasks in this window yet. ";
      return `<p class="tq-empty">${where}Plan a feature with one of its repos' Claudes, then ask it to file the plan: ` +
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
        (t.marked ? `<span class="tq-look" title="Its builder lists something a person must check before it ships. It merges on its own all the same.">For you</span>` : "") +
        (t.note ? `<span class="tq-note">${esc(L().preview(t.note))}</span>` : "") + `</div>` +
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
      return `<div class="tq-detail">${forYouHtml(t)}${title}${note}${(t.parts || []).map((p) => partHtml(t, p)).join("")}` +
        `<div class="tq-meta">filed by ${esc(t.filedBy || "?")}</div>` +
        `<div class="tq-actions">${buttonsHtml(t)}</div></div>`;
    }

    // forYouHtml is the small box of what a task's builders say a person
    // must check, and why: at most three short lines a part. Once a person
    // has marked them read the box stays, quieter, for the record.
    function forYouHtml(t) {
      const lines = L().forYou(t);
      if (!lines.length) return "";
      return `<div class="tq-foryou${t.marked ? "" : " read"}"><div class="tq-foryou-head">For you${t.marked ? "" : " · read"}</div>` +
        `<ul>${lines.map((l) => `<li>${esc(l)}</li>`).join("")}</ul></div>`;
    }

    // reportHtml is a part's report, folded away under "Full report" and
    // read as Markdown; it stays open across redraws once a person opens it.
    function reportHtml(p) {
      if (!p.note) return "";
      const open = q.reports.has(p.id) ? " open" : "";
      return `<details class="tq-report" data-report="${p.id}"${open}><summary>Full report</summary>` +
        `<div class="md">${L().reportHtml(MD().parse(p.note), esc)}</div></details>`;
    }

    function partHtml(t, p) {
      const plan = t.editable
        ? `<textarea class="tq-edit-plan" data-field="plan" data-key="p${p.id}" spellcheck="false">${esc(draft("p" + p.id, p.plan))}</textarea>`
        : `<pre class="tq-plan">${esc(p.plan)}</pre>`;
      const where = p.branch ? ` · ${esc(p.busName)} on ${esc(p.branch)}` : "";
      const run = L().builtWith(p);
      const b = (act, label) => `<button type="button" data-act="${act}">${esc(label)}</button>`;
      const buttons = (p.workspaceId || p.sessionId ? b("session", "Open session") : "") + (p.retryable ? b("retry", "Retry") : "") +
        (p.sendBackable ? b("sendback", "Send back to worker") : "");
      return `<div class="tq-part" data-part="${p.id}"><div class="tq-part-head">` +
        `<span class="tq-repo-chip tq-part-${esc(p.status)}">${esc(p.repo)}</span>` +
        `<span class="tq-part-status">${esc(L().partWord(p.status))}</span>` +
        (run ? `<span class="tq-meta" title="The model and effort this part's Claude starts with">${esc(run)}</span>` : "") +
        (p.note ? `<span class="tq-note">${esc(L().preview(p.note))}</span>` : "") +
        `<span class="tq-meta">${where}</span>${buttons ? `<span class="tq-actions">${buttons}</span>` : ""}</div>${reportHtml(p)}${plan}</div>`;
    }

    function buttonsHtml(t) {
      const b = (act, label, extra) => `<button type="button" data-act="${act}"${extra || ""}>${esc(label)}</button>`;
      const out = [];
      if (t.editable) out.push(b("save", "Save"));
      if (t.canUp) out.push(b("up", "↑"));
      if (t.canDown) out.push(b("down", "↓"));
      for (const dep of (t.orderable && t.afterIds) || []) out.push(b("unwait", `Stop waiting on #${dep}`, ` data-to="${dep}"`));
      if (t.marked) out.push(b("read", "Mark as read"));
      for (const to of t.moves || []) out.push(b("move", L().moveLabel(to), ` data-to="${to}"`));
      if (t.removable) out.push(b("remove", "Remove"));
      return out.join("");
    }

    // --- acting ---

    // The header's pickers, by their data-act; the rest act on a task.
    const PICKERS = new Set(["repo", "slots", "priority"]);

    // wire hangs the handlers on the box once; every redraw reuses them.
    function wire(b) {
      if (b.dataset.wired) return;
      b.dataset.wired = "1";
      b.addEventListener("click", onClick);
      b.addEventListener("input", onInput);
      b.addEventListener("change", (e) => {
        const kind = e.target.dataset.act;
        if (kind === "repo") { q.repo = e.target.value; render(); }
        if (kind === "slots" || kind === "priority") setNumber(kind, Number(e.target.value));
      });
      // A report's <details> keeps its open state across redraws. toggle
      // does not bubble, so it is caught on the way down.
      b.addEventListener("toggle", (e) => {
        const id = Number(e.target.dataset && e.target.dataset.report);
        if (!id) return;
        if (e.target.open) q.reports.add(id); else q.reports.delete(id);
      }, true);
      b.addEventListener("focusout", () => setTimeout(() => { if (q.pending) render(); }, 0));
    }

    function onInput(e) {
      const key = e.target.dataset.key;
      if (key) q.drafts.set(key, e.target.value);
    }

    function onClick(e) {
      const btn = e.target.closest("[data-act]"), el = e.target.closest(".tq-task");
      if (!btn || !el || PICKERS.has(btn.dataset.act)) return;
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

    // unwait drops one task (the button's data-to) from what a task starts
    // after. The rest of the list stays: it still waits on those.
    function unwait(id, dep) {
      const t = q.tasks.find((x) => x.id === id);
      if (t) send(`/${id}`, "PATCH", { afterIds: (t.afterIds || []).filter((a) => a !== Number(dep)) });
    }

    // openSession shows a part's worker session; the page leaves this view.
    function openSession(id, _to, partId) {
      const t = q.tasks.find((x) => x.id === id);
      const p = t && (t.parts || []).find((x) => x.id === partId);
      if (p && (p.workspaceId || p.sessionId)) host.openSession(p.workspaceId, p.paneId, p.sessionId);
    }

    // What each button does, by its data-act.
    const actions = {
      toggle,
      save,
      remove,
      up: (id) => send(`/${id}/move`, "POST", { dir: "up" }),
      down: (id) => send(`/${id}/move`, "POST", { dir: "down" }),
      move: (id, to) => send(`/${id}`, "PATCH", { status: to }),
      unwait,
      // A person has read what its builders listed: the mark goes, the lines stay.
      read: (id) => send(`/${id}`, "PATCH", { read: true }),
      session: openSession,
      retry: (id, _to, partId) => send(`/${id}/parts/${partId}`, "PATCH", { retry: true }),
      // The worker gets the task's failure note and the part runs again.
      sendback: (id, _to, partId) => send(`/${id}/parts/${partId}/send-back`, "POST"),
    };

    function act(kind, id, to, partId) {
      if (Object.hasOwn(actions, kind)) actions[kind](id, to, partId);
    }

    return {
      refresh, open, close,
      isOpen: () => !!q.win, openWindow: () => q.win, openRepo: () => (q.win ? q.repo : ""),
      counts: (winId) => q.counts[winId] || {}, tasks: () => q.tasks,
    };
  }

  window.ccmuxTaskView = { create };
})();
