// threadlive.js: what a running session is doing besides its conversation,
// on the thread page, as Claude Code shows it around its prompt box:
// - its permission mode, switched like Shift+Tab (POST .../mode);
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

  const kinds = { local_bash: "Shell", local_agent: "Agent", monitor: "Monitor", workflow: "Workflow" };

  // taskLabel is one background task as its row says it.
  function taskLabel(t) {
    const kind = kinds[t.type] || "Task";
    return `${t.agent ? `${kind} (${t.agent})` : kind}: ${t.description || t.id}`;
  }

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

    function drawTasks(tasks) {
      const bar = $("bgtasks-bar");
      show("bgtasks-bar", tasks.length > 0);
      bar.textContent = "";
      if (!tasks.length) return;
      const head = document.createElement("span");
      head.className = "bg-head";
      head.textContent = tasks.length === 1 ? "Running in the background:" : `${tasks.length} running in the background:`;
      bar.appendChild(head);
      for (const t of tasks) bar.appendChild(taskRow(t));
    }

    function taskRow(t) {
      const row = document.createElement("span");
      row.className = "bg-task";
      const name = document.createElement("span");
      name.className = "bg-name";
      name.textContent = taskLabel(t);
      const stop = document.createElement("button");
      stop.type = "button";
      stop.className = "plain small bg-stop";
      stop.textContent = "Stop";
      stop.onclick = () => act(`/tasks/${enc(t.id)}/stop`, undefined, stop);
      row.append(name, stop);
      return row;
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
    return { draw };
  }

  const api = { create, MODES, taskLabel, limitLine };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  if (typeof window !== "undefined") window.ccmuxThreadLive = api;
})();
