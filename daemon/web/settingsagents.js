// settingsagents.js: the Settings window's Agents tab. Base agents are
// folders the daemon owns, listed here; New and Edit open the editor
// (agentmodal.js), which does the PUT. Delete is explicit and confirmed: the
// folder holds skills a human wrote.
//
// A page that keeps its own list of agents (the lens's window menu does)
// hears "ccmux-agents-changed" on document whenever this tab read or
// changed the bases, and drops what it had; this file knows nothing of it.
(() => {
  "use strict";
  const S = window.ccmuxSettings;

  S.addTab({
    page: "agents", label: "Agents", opened: load,
    html:
      `<p class="hint setting-hint">Role agents live as folders under <code>~/.ccmux/agents</code>. Edit one to set its role, skills and tools; add it to a project from the window's ⚙ menu.</p>` +
      `<div id="agent-list" class="rules-list"></div>` +
      `<button id="agent-add" class="rule-add" type="button">+ New agent</button>` +
      `<p id="agent-state" class="hint setting-hint"></p>` +
      `<p id="agent-sdk" class="hint setting-hint"></p>`,
  });
  const box = $("agent-list"), addBtn = $("agent-add");
  const say = (text) => { $("agent-state").textContent = text; };
  const changed = () => document.dispatchEvent(new Event("ccmux-agents-changed"));
  let cfg = {};

  function agentRow(a) {
    const row = document.createElement("div");
    row.className = "entry-card agent-card";
    row.innerHTML =
      `<div class="entry-line">` +
      `<span class="agent-icon">${esc(a.icon || "")}</span>` +
      `<span class="agent-name">${esc(a.name)}</span>` +
      `<span class="agent-version">v${esc(a.version || "")}</span>` +
      `<span class="agent-harness">${esc(a.harness || "")}</span>` +
      `<span class="grow"></span>` +
      `<button class="rule-add ag-edit" type="button">Edit</button>` +
      `<button class="rule-del" type="button" title="Delete agent and its folder">&times;</button>` +
      `</div>` +
      `<div class="agent-desc">${esc(a.description || "")}</div>`;
    row.querySelector(".ag-edit").onclick = () => window.ccmuxAgentModal.open(a, cfg, load);
    row.querySelector(".rule-del").onclick = () => deleteAgent(a.name);
    return row;
  }

  async function deleteAgent(name) {
    if (!confirm(`Delete agent "${name}" and its folder (skills included)? Project instance folders stay.`)) return;
    try {
      const r = await fetch(`/v1/agents/${encodeURIComponent(name)}`, { method: "DELETE" });
      if (!r.ok && r.status !== 404) throw new Error(await r.text());
    } catch (e) {
      say("Not deleted: " + e.message);
      return;
    }
    say(`Deleted ${name}.`);
    changed();
    load();
  }

  // sdkLine is the "agents on X, latest Y" line: what sessions and claude
  // agents start on (a newer version the daily update tested, else the
  // pin), how the last update went when it failed, and the newest on npm
  // when it differs.
  function sdkLine(v) {
    if (!v || !v.pinned) return "";
    const runs = v.current || v.pinned;
    let line = `Sessions and Claude agents start on Agent SDK ${runs}`;
    if (v.current) line += ` (taken by the daily update; this daemon pins ${v.pinned})`;
    else if (v.installed && v.installed !== v.pinned) line += ` (installed ${v.installed}; ${v.pinned} lands at the next claude start)`;
    const u = v.update || {};
    if (u.version && !u.ok) line += ` · the update to ${u.version} failed (${u.kind === "test" ? "a test session on it did not work" : "it could not be installed"}): ${u.error || "no reason given"}`;
    if (v.latest && v.latest !== runs) line += ` · newest is ${v.latest}`;
    return line + ".";
  }

  // Draws what GET /v1/agents answered: the rows, or why there are none.
  async function draw(r) {
    box.innerHTML = "";
    if (r.status === 503) { say("This daemon has no agents support."); addBtn.disabled = true; return; }
    addBtn.disabled = false;
    if (!r.ok) { say("Couldn't load agents: " + (await r.text())); return; } // names the broken folder
    const answer = await r.json();
    const list = answer.agents || [];
    $("agent-sdk").textContent = sdkLine(answer.sdk);
    for (const a of list) box.appendChild(agentRow(a));
    say(list.length ? "" : "No agents yet.");
    changed();
  }

  async function load() {
    try {
      const [c, r] = await Promise.all([(await fetch("/v1/settings")).json(), fetch("/v1/agents")]);
      cfg = c;
      await draw(r);
    } catch (_) {
      say("Couldn't load agents.");
    }
  }

  addBtn.addEventListener("click", () => window.ccmuxAgentModal.open(null, cfg, load));
})();
