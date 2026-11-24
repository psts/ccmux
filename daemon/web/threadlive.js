// threadlive.js: what a running session is doing besides its conversation,
// on the thread page, as Claude Code shows it around its prompt box:
// - its permission mode, switched like Shift+Tab (POST .../mode);
// - its effort, set like /effort (POST .../effort), which its row keeps
//   for later starts too;
// - its background tasks (a dev server, a monitor, a helper sent off),
//   each with Stop (POST .../tasks/{id}/stop);
// - a warning when the plan's usage limit is near or reached.
// The runner says all three in what the session says of itself
// (session.info: permissionMode, tasks, limit; claudeserve/sessionself.mjs).
(() => {
  "use strict";
  const enc = encodeURIComponent;

  // MODES are the ones the runner switches to, as the page names them.
  const MODES = [
    { value: "default", label: "Ask first" }, { value: "acceptEdits", label: "Accept edits" },
    { value: "plan", label: "Plan" }, { value: "auto", label: "Auto" },
  ];

  // EFFORTS are the levels a session may be set to, "" the model's own
  // (api/sessioneffort.go efforts).
  const EFFORTS = [
    { value: "", label: "Default" }, { value: "low", label: "Low" }, { value: "medium", label: "Medium" },
    { value: "high", label: "High" }, { value: "xhigh", label: "Extra high" }, { value: "max", label: "Max" },
  ];

  const effortLabel = (value) => (EFFORTS.find((e) => e.value === value) || { label: value }).label;

  // effortOptions are the levels to offer: the ones the session's model
  // takes when its runner said (info.models efforts), else all; the one
  // picked now is always there. Default names the level the session's
  // settings give its model, when its runner said (info.settingsEffort,
  // claudeserve/sessionself.mjs settingsEffort).
  function effortOptions(info, picked) {
    const models = (info && info.models) || [];
    const m = models.find((x) => x.model === info.model || x.value === info.model);
    const takes = m && Array.isArray(m.efforts) ? m.efforts : null;
    const given = info && info.settingsEffort ? ` (${effortLabel(info.settingsEffort)})` : "";
    return EFFORTS.filter((e) => !e.value || e.value === picked || !takes || takes.includes(e.value))
      .map((e) => (e.value ? e : { ...e, label: e.label + given }));
  }

  const kinds = { local_bash: "Shell", local_agent: "Agent", monitor: "Monitor", workflow: "Workflow" };

  // taskParts is one background task as the tray says it: what kind, its
  // own words, and the agent that runs it, apart.
  const taskParts = (t) => ({ kind: kinds[t.type] || "Task", name: t.description || t.id, agent: t.agent || "" });

  // ranFor is how long a task has run, from when its runner first saw it
  // (since, claudeserve/sessionself.mjs), in whole minutes; "" when the
  // runner kept no time (one from before it did).
  function ranFor(since, now) {
    if (!since) return "";
    const m = Math.floor(Math.max(0, now - since) / 60000);
    if (m < 1) return "<1m";
    if (m < 60) return m + "m";
    return Math.floor(m / 60) + "h" + (m % 60 ? " " + (m % 60) + "m" : "");
  }

  // trayLine is the folded tray's words for several tasks: the first two,
  // then how many more.
  function trayLine(tasks) {
    const names = tasks.slice(0, 2).map((t) => taskParts(t).name).join(", ");
    return tasks.length > 2 ? `${names}, +${tasks.length - 2}` : names;
  }

  // trayKey is what the tray shows, as one string: whether it is open and
  // each task's id, words, kind, agent and start.
  const trayKey = (tasks, open) => JSON.stringify([open, tasks.map((t) => [t.id, t.description, t.type, t.agent, t.since])]);

  const limitNames = { five_hour: "5-hour", seven_day: "weekly", seven_day_opus: "weekly Opus", seven_day_sonnet: "weekly Sonnet", overage: "extra usage" };

  // limitLine is the warning for a usage limit the runner reported, "" for
  // none. resetsAt is in seconds (Claude Code's), or milliseconds.
  function limitLine(limit, clock) {
    if (!limit || !limit.status || limit.status === "allowed") return "";
    const name = limitNames[limit.type] ? `${limitNames[limit.type]} ` : "";
    const used = typeof limit.utilization === "number" ? ` (${Math.round(limit.utilization * 100)}% used)` : "";
    const at = limit.resetsAt ? ` It resets at ${clock(limit.resetsAt < 1e12 ? limit.resetsAt * 1000 : limit.resetsAt)}.` : "";
    if (limit.status === "rejected") return `You have reached your ${name}usage limit.${at}`;
    return `You are near your ${name}usage limit${used}.${at}`;
  }

  // create wires it to the page: d.call is its fetch, d.session() the
  // session on screen (or null), d.info() what it says of itself,
  // d.running() whether it runs now, d.fail(text) shows what went wrong,
  // d.clock(ms) says a time of day.
  function create(d) {
    const $ = (id) => document.getElementById(id);
    const show = (id, on) => $(id).classList.toggle("hidden", !on);

    function draw() {
      const sess = d.session();
      const live = !!sess && d.running();
      const info = d.info() || {};
      drawEffort(sess, live, info); // before drawMode, whose pill sync shows its label
      drawMode(live, info.permissionMode || "");
      drawTasks(live ? info.tasks || [] : []);
      const warn = sess ? limitLine(info.limit, d.clock) : "";
      show("limit-bar", !!warn);
      $("limit-bar").textContent = warn;
    }

    function drawMode(live, mode) {
      const select = $("mode");
      if (!select.dataset.filled) {
        select.dataset.filled = "1";
        for (const m of MODES) { const o = document.createElement("option"); o.value = m.value; o.textContent = m.label; select.appendChild(o); }
      }
      const known = MODES.some((m) => m.value === mode);
      show("mode-box", live && known);
      if (known) select.value = mode;
      if (window.ccmuxThreadPill) window.ccmuxThreadPill.sync($);
    }

    // drawEffort is the Effort picker: for a running session that is not a
    // role agent's (its base names its effort). Its options are filled
    // again only when they change, so an open list is not pulled away.
    function drawEffort(sess, live, info) {
      const select = $("effort");
      show("effort-box", live && sess.kind !== "agent");
      if (!live) return;
      const picked = sess.effort || "";
      const opts = effortOptions(info, picked);
      const key = opts.map((o) => o.value + ":" + o.label).join(",");
      if (select.dataset.key !== key) {
        select.dataset.key = key;
        select.textContent = "";
        for (const o of opts) { const n = document.createElement("option"); n.value = o.value; n.textContent = o.label; select.appendChild(n); }
      }
      select.value = picked;
    }

    // pickEffort saves the session's effort; a runner that could not take
    // it at once (one started before it could) gets it at its restart.
    async function pickEffort(value) {
      const sess = d.session();
      if (!sess) return;
      try {
        const out = await d.call("POST", `/v1/sessions/${enc(sess.id)}/effort`, { effort: value });
        sess.effort = value;
        const label = (effortOptions(d.info() || {}, value).find((o) => o.value === value) || { label: value }).label;
        if (out && out.applies && out.applies !== "now") d.said(`Effort ${label} is saved and applies when the session restarts.`);
        else draw();
      } catch (e) {
        d.fail(e.message);
      }
    }

    // drawTasks is the tray on the chat box (threads.css .bgtasks): one
    // task on its one line with its Stop; several folded into one line that
    // opens to a row each. Open or folded holds while the page is open.
    // The tray is drawn anew only when what it says changed (trayKey): the
    // page calls draw on every stream event and every lists' reload, and a
    // rebuild drops the keyboard's place and enables again a Stop whose
    // request is on its way. In between, tick moves its minutes on.
    let trayOpen = false;
    let drawnKey = null;
    function drawTasks(tasks) {
      const key = trayKey(tasks, trayOpen);
      if (key === drawnKey) return tick();
      drawnKey = key;
      const bar = $("bgtasks-bar");
      show("bgtasks-bar", tasks.length > 0);
      bar.textContent = "";
      if (!tasks.length) return;
      const now = Date.now();
      if (tasks.length === 1) {
        bar.className = "bgtasks one";
        bar.appendChild(taskRow(tasks[0], now, "bg-head"));
        return;
      }
      bar.className = "bgtasks" + (trayOpen ? " open" : "");
      const head = el("button", "bg-head");
      head.type = "button";
      head.setAttribute("aria-expanded", String(trayOpen));
      head.onclick = () => { trayOpen = !trayOpen; draw(); $("bgtasks-bar").querySelector(".bg-head").focus(); }; // the line was drawn anew: keep the keyboard on it
      head.append(el("span", "bg-dot"), el("b", "", `${tasks.length} running`), el("span", "bg-sep", " · "), el("span", "bg-what", trayLine(tasks)), el("span", "bg-chev", "⌃"));
      bar.appendChild(head);
      if (!trayOpen) return;
      const list = el("div", "bg-list");
      for (const t of tasks) list.appendChild(taskRow(t, now, "bg-row"));
      bar.appendChild(list);
    }

    // taskRow is one task: its kind, its words and agent, how long it has
    // run, and Stop.
    function taskRow(t, now, cls) {
      const p = taskParts(t);
      const row = el("div", cls);
      if (cls === "bg-head") row.appendChild(el("span", "bg-dot"));
      const stop = el("button", "plain small bg-stop", "Stop");
      stop.type = "button";
      stop.onclick = () => act(`/tasks/${enc(t.id)}/stop`, undefined, stop);
      const time = el("span", "bg-time", ranFor(t.since, now));
      time.dataset.since = String(t.since || "");
      row.append(el("span", "bg-kind", p.kind), el("span", "bg-name", p.agent ? `${p.name} · ${p.agent}` : p.name), time, stop);
      return row;
    }

    // tick moves the tray's minutes on in place: a rebuild would drop the
    // keyboard's place and enable again a Stop whose request is on its way.
    function tick() {
      const now = Date.now();
      for (const t of $("bgtasks-bar").querySelectorAll(".bg-time")) t.textContent = ranFor(Number(t.dataset.since) || 0, now);
    }

    function el(tag, cls, text) {
      const n = document.createElement(tag);
      if (cls) n.className = cls;
      if (text !== undefined) n.textContent = text;
      return n;
    }

    // act asks the session's runner for one thing; the next session.info
    // shows what came of it.
    async function act(path, body, button) {
      const sess = d.session();
      if (!sess) return;
      if (button) button.disabled = true;
      try {
        await d.call("POST", `/v1/sessions/${enc(sess.id)}${path}`, body);
      } catch (e) {
        d.fail(e.message);
        draw();
      } finally {
        if (button) button.disabled = false;
      }
    }

    $("mode").onchange = (e) => act("/mode", { mode: e.target.value });
    $("effort").onchange = (e) => pickEffort(e.target.value);
    return { draw, tick };
  }

  const api = { create, MODES, EFFORTS, effortOptions, taskParts, ranFor, trayLine, trayKey, limitLine };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  if (typeof window !== "undefined") window.ccmuxThreadLive = api;
})();
