// settingssandbox.js: the Settings window's Sandbox tab (GET/PUT
// /v1/sandbox, POST /v1/sandbox/check). Two things live here: the caps every
// session and job service runs under (memory, CPU, processes, a job's
// hours), and whether task and thread work is walled into its worktree.
// Under them, what this host allows, with the command that fixes what it
// lacks. The bounds are the daemon's (walls.Settings.Validate): a value it
// refuses comes back as its own sentence. Loads after settingsmodal.js,
// which gives it its tab, and dom.js, whose $ it uses.
(function (root) {
  "use strict";

  // hostLines is what the host allows, one line each, worst news in plain
  // words: {ok, text}. No host (a failed read) is no lines.
  function hostLines(host) {
    if (!host || typeof host !== "object") return [];
    const why = (text) => (text ? ": " + text : "");
    return [
      { ok: !!host.caps, text: host.caps ? "Caps work: memory, CPU and process limits hold." : "Caps do not hold" + why(host.capsWhy) },
      { ok: !!host.walls, text: host.walls ? "Walls work: a trial unit ran behind them." : "Walls do not work here" + why(host.wallsWhy) },
      { ok: !!host.bwrap, text: host.bwrap ? "bubblewrap works (Claude Code's own sandbox needs it)." : "bubblewrap does not work" + why(host.bwrapWhy) },
      { ok: !!host.socat, text: host.socat ? "socat is installed (Claude Code's own sandbox needs it)." : "socat is not installed." },
    ];
  }

  // extraPaths reads the folders box: one per line, blanks dropped.
  function extraPaths(text) {
    return String(text || "").split("\n").map((l) => l.trim()).filter(Boolean);
  }

  // formSettings is the form as the daemon takes it. A number box that
  // holds no number is sent as 0, which the daemon refuses with the bounds.
  function formSettings(f) {
    const n = (v) => (Number.isFinite(Number(v)) && String(v).trim() !== "" ? Math.trunc(Number(v)) : 0);
    return {
      on: !!f.on, shellBox: !!f.shellBox,
      memoryPercent: n(f.memoryPercent), cpuCores: n(f.cpuCores), tasksMax: n(f.tasksMax), jobHours: n(f.jobHours),
      extraWrite: extraPaths(f.extraWrite),
    };
  }

  // verdict is the one line under the switches: what happens to task and
  // thread work as things stand.
  function verdict(settings, host) {
    if (!settings || !host) return "";
    if (!settings.on) return "Off: task and thread work runs under the caps only, and can write wherever you can.";
    if (!host.walls) return "On, and this host cannot wall a unit: task and thread work will not start until the host is fixed (below) or this is switched off.";
    if (settings.shellBox && !(host.bwrap && host.socat)) return "On, and the sandbox for shell commands is on too, which this host cannot run: walled sessions will not start until that is fixed or switched off.";
    return "On: each builder, thread member and merge writes only in its own worktree and the caches. Applies from each one's next start.";
  }

  const api = { hostLines, extraPaths, formSettings, verdict };
  if (typeof module !== "undefined" && module.exports) { module.exports = api; return; }
  root.ccmuxSandboxSettings = api;

  const num = (id, label) =>
    `<div class="entry-line"><label class="entry-label grow" for="${id}">${label}</label>` +
    `<input id="${id}" class="setting-input" type="number" inputmode="numeric" style="width:9em"></div>`;
  root.ccmuxSettings.addTab({
    page: "sandbox", label: "Sandbox", opened: load,
    html:
      `<label class="setting-label">Walls</label>` +
      `<label class="sandbox-switch" for="sandbox-on"><input id="sandbox-on" type="checkbox">Wall task and thread work into its worktree</label>` +
      `<label class="sandbox-switch" for="sandbox-shell"><input id="sandbox-shell" type="checkbox">Also run their shell commands in Claude Code's own sandbox</label>` +
      `<p id="sandbox-verdict" class="hint setting-hint"></p>` +
      `<p class="hint setting-hint">The second switch costs something: a sandboxed command reaches no loopback address (not a builder's own dev servers) and only the hosts your Claude settings allow (sandbox.network.allowedDomains).</p>` +
      `<label class="setting-label">Caps, for every session and job</label>` +
      num("sandbox-mem", "Memory, percent of the machine") +
      num("sandbox-cpu", "CPU, cores") +
      num("sandbox-tasks", "Processes and threads") +
      num("sandbox-hours", "A job's longest run, hours") +
      `<p class="hint setting-hint">The caps take hold at once, on what is running too.</p>` +
      `<label class="setting-label" for="sandbox-extra">Extra folders walled work may write, one per line</label>` +
      `<textarea id="sandbox-extra" class="setting-input" rows="3" spellcheck="false" placeholder="/home/you/.cargo"></textarea>` +
      `<p class="hint setting-hint">Always open: the worktree, the Go, npm and ~/.cache caches, and for a session Claude Code's own folder.</p>` +
      `<div class="sandbox-actions"><button id="sandbox-check" class="rule-add" type="button">Check this host again</button><button id="sandbox-save" class="rule-add" type="button">Save</button></div>` +
      `<p id="sandbox-state" class="hint setting-hint"></p>` +
      `<label class="setting-label">This host</label>` +
      `<div id="sandbox-host" class="rules-list"></div>`,
  });
  const el = {
    on: $("sandbox-on"), shell: $("sandbox-shell"), mem: $("sandbox-mem"), cpu: $("sandbox-cpu"),
    tasks: $("sandbox-tasks"), hours: $("sandbox-hours"), extra: $("sandbox-extra"),
    save: $("sandbox-save"), check: $("sandbox-check"), state: $("sandbox-state"),
    verdict: $("sandbox-verdict"), host: $("sandbox-host"),
  };
  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));

  // drawHost lists what the host allows, and the commands for what it
  // lacks.
  function drawHost(host) {
    const fix = (host && host.fix) || [];
    el.host.innerHTML =
      hostLines(host).map((l) => `<div>${l.ok ? "✓" : "✗"} ${esc(l.text)}</div>`).join("") +
      (fix.length ? `<div>Run this, then check again:</div>` + fix.map((c) => `<pre class="setting-input sandbox-fix">${esc(c)}</pre>`).join("") : "");
  }

  // draw shows what the daemon answered. Without settings (a failed read)
  // the form is locked: a save would write its blanks back.
  function draw(view) {
    const s = view && view.settings;
    for (const box of [el.on, el.shell, el.mem, el.cpu, el.tasks, el.hours, el.extra, el.save]) box.disabled = !s;
    if (!s) return;
    el.on.checked = !!s.on;
    el.shell.checked = !!s.shellBox;
    el.mem.value = s.memoryPercent;
    el.cpu.value = s.cpuCores;
    el.tasks.value = s.tasksMax;
    el.hours.value = s.jobHours;
    el.extra.value = (s.extraWrite || []).join("\n");
    el.verdict.textContent = verdict(s, view.host);
    drawHost(view.host);
  }

  async function ask(method, url, body) {
    const r = await fetch(url, body === undefined ? { method } : { method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    const got = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(got.error || `HTTP ${r.status}`);
    return got;
  }

  async function load() {
    try {
      draw(await ask("GET", "/v1/sandbox"));
      el.state.textContent = "";
    } catch (e) {
      draw(null);
      el.state.textContent = "Couldn't read the sandbox settings: " + e.message;
    }
  }

  el.save.addEventListener("click", async () => {
    const want = formSettings({
      on: el.on.checked, shellBox: el.shell.checked, memoryPercent: el.mem.value, cpuCores: el.cpu.value,
      tasksMax: el.tasks.value, jobHours: el.hours.value, extraWrite: el.extra.value,
    });
    try {
      draw(await ask("PUT", "/v1/sandbox", want));
      el.state.textContent = "Saved.";
    } catch (e) {
      el.state.textContent = "Couldn't save: " + e.message;
    }
  });

  el.check.addEventListener("click", async () => {
    el.state.textContent = "Checking…";
    try {
      const view = await ask("POST", "/v1/sandbox/check");
      // Only the host: what is typed and not saved stays as it is.
      const typed = el.save.disabled ? null : view.settings;
      el.verdict.textContent = typed ? verdict(typed, view.host) : "";
      drawHost(view.host);
      el.state.textContent = "Checked.";
    } catch (e) {
      el.state.textContent = "Couldn't check: " + e.message;
    }
  });
})(typeof window !== "undefined" ? window : this);
