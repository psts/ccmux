// Agent editor, capabilities section: what an isolated claude agent loads.
// Isolated, it reads none of this host's own Claude Code setup (user
// settings, hooks, plugins, MCP servers, claude.ai connectors), only its
// base plus the catalog entries ticked here. The catalog is one list for
// the host (/v1/capabilities): a plugin, or one MCP server. Skills from the
// base and the ticked plugins can be switched off one by one, and the tool
// rules say what runs without a card and what never runs. All of it is
// saved with the form and applies at the agent's next wake. Same fields as
// the Mac editor (AgentCapabilitiesView.swift).
(() => {
  const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
  let st = { catalog: [], installed: [], picked: new Set(), off: new Set(), baseSkills: [], agent: "" };
  let root = null;
  const q = (sel) => root.querySelector(sel);

  function html() {
    return `<div class="entry-line"><label class="hx-confirm"><input type="checkbox" class="cap-isolated"> isolated: load only what is ticked here, not this host's Claude setup (claude harness; applies at the next wake)</label></div>` +
      `<div class="cap-catalog rules-list"></div>` +
      `<label class="am-lbl">skills (untick to switch one off)</label><div class="cap-skills cap-checks"></div>` +
      `<label class="am-lbl">tools that run without a card, one rule per line</label>` +
      `<textarea class="setting-input cap-allow" rows="3" spellcheck="false" placeholder="mcp__plugin_chartlabs-kb_kb-api__kb_get_doc"></textarea>` +
      `<label class="am-lbl">tools that never run</label>` +
      `<textarea class="setting-input cap-deny" rows="2" spellcheck="false" placeholder="AskUserQuestion"></textarea>` +
      `<details class="cap-add"><summary class="am-lbl">add to the catalog</summary>` +
      `<div class="entry-line"><input class="setting-input cap-id" placeholder="id" spellcheck="false"><select class="setting-input cap-kind"><option value="plugin">installed plugin</option><option value="path">plugin folder</option><option value="mcp">MCP server</option></select></div>` +
      `<div class="entry-line"><select class="setting-input cap-plugin grow"></select><input class="setting-input cap-path grow hidden" placeholder="/absolute/path/to/plugin" spellcheck="false"></div>` +
      `<textarea class="setting-input cap-server hidden" rows="3" spellcheck="false" placeholder='{"type": "http", "url": "http://127.0.0.1:8930/x/mcp", "headers": {"Authorization": "Bearer \${TOKEN}"}}'></textarea>` +
      `<div class="entry-line"><button class="rule-add cap-save" type="button">Add</button><button class="rule-add cap-import" type="button">Import from this host</button></div></details>` +
      `<p class="hint cap-state"></p>`;
  }

  // mount builds the section into box once.
  function mount(box) {
    root = box;
    box.innerHTML = html();
    q(".cap-kind").onchange = showKind;
    q(".cap-save").onclick = addEntry;
    q(".cap-import").onclick = importHost;
  }

  function showKind() {
    const k = q(".cap-kind").value;
    q(".cap-plugin").classList.toggle("hidden", k !== "plugin");
    q(".cap-path").classList.toggle("hidden", k !== "path");
    q(".cap-server").classList.toggle("hidden", k !== "mcp");
  }

  // fill sets the form from agent a and loads the catalog and its skills.
  async function fill(a) {
    st = { catalog: [], installed: [], picked: new Set(a.capabilities || []), off: new Set(a.skillsOff || []), baseSkills: [], agent: a.name || "" };
    q(".cap-isolated").checked = !!a.isolated;
    q(".cap-allow").value = (a.toolAllow || []).join("\n");
    q(".cap-deny").value = (a.toolDeny || []).join("\n");
    q(".cap-state").textContent = "";
    showKind();
    await Promise.all([loadCatalog(), loadBaseSkills()]);
  }

  async function loadCatalog() {
    try {
      const r = await fetch("/v1/capabilities");
      if (!r.ok) throw new Error(await r.text());
      const body = await r.json();
      st.catalog = body.capabilities || [];
      st.installed = body.installed || [];
    } catch (e) { q(".cap-state").textContent = "Couldn't load the catalog: " + e.message; }
    q(".cap-plugin").innerHTML = st.installed.map((id) => `<option value="${esc(id)}">${esc(id)}</option>`).join("");
    renderCatalog();
  }

  async function loadBaseSkills() {
    if (!st.agent) return;
    try {
      const r = await fetch(`/v1/agents/${encodeURIComponent(st.agent)}/skills`);
      st.baseSkills = r.ok ? ((await r.json()).skills || []).map((s) => `${st.agent}:${s.name}`) : [];
    } catch (_) { st.baseSkills = []; }
    renderSkills();
  }

  function renderCatalog() {
    const box = q(".cap-catalog");
    box.innerHTML = "";
    if (!st.catalog.length) box.appendChild(el("p", "hint", "The catalog is empty. Add an entry below, or import what this host's agents use today."));
    for (const c of st.catalog) box.appendChild(catalogRow(c));
    renderSkills();
  }

  function catalogRow(c) {
    const row = el("div", "entry-card am-row");
    const line = el("div", "entry-line");
    const tick = el("input");
    tick.type = "checkbox";
    tick.checked = st.picked.has(c.id);
    tick.onchange = () => { tick.checked ? st.picked.add(c.id) : st.picked.delete(c.id); renderSkills(); };
    line.appendChild(tick);
    const text = el("div", "grow");
    text.appendChild(el("div", "am-row-title", c.id));
    text.appendChild(el("div", "am-row-sub", describe(c)));
    if ((c.sideEffects || []).length) text.appendChild(el("div", "am-row-src", "side effects: " + c.sideEffects.join(", ")));
    line.appendChild(text);
    const del = el("button", "rule-del", "×");
    del.title = "Remove from the catalog";
    del.onclick = () => removeEntry(c.id);
    line.appendChild(del);
    row.appendChild(line);
    return row;
  }

  // describe names what an entry is, never a secret: a server shows its url
  // or command and only the names of its env and headers.
  function describe(c) {
    if (c.kind === "plugin") return "plugin " + (c.plugin || c.path);
    const s = c.server || {};
    const where = s.url ? `${s.type || "http"} ${s.url}` : [s.command].concat(s.args || []).join(" ");
    const names = Object.keys(s.env || {}).concat(Object.keys(s.headers || {}));
    return "MCP " + where + (names.length ? " · " + names.join(", ") : "");
  }

  // renderSkills lists the base's skills and those of every ticked plugin.
  function renderSkills() {
    const box = q(".cap-skills");
    box.innerHTML = "";
    const names = st.baseSkills.concat(...st.catalog.filter((c) => st.picked.has(c.id)).map((c) => c.skills || []));
    if (!names.length) { box.appendChild(el("span", "hint", "No skills.")); return; }
    for (const n of names) {
      const lbl = el("label", "hx-confirm");
      const tick = el("input");
      tick.type = "checkbox";
      tick.checked = !st.off.has(n);
      tick.onchange = () => { tick.checked ? st.off.delete(n) : st.off.add(n); };
      lbl.append(tick, " " + n);
      box.appendChild(lbl);
    }
  }

  // value is the section's part of the agent PUT.
  function value() {
    const lines = (sel) => q(sel).value.split("\n").map((s) => s.trim()).filter(Boolean);
    return {
      isolated: q(".cap-isolated").checked,
      // An id the catalog lost stays listed (it shows no row to untick):
      // dropping it quietly would hide why the next start fails.
      capabilities: [...st.picked],
      skillsOff: [...st.off],
      toolAllow: lines(".cap-allow"),
      toolDeny: lines(".cap-deny"),
    };
  }

  async function addEntry() {
    const id = q(".cap-id").value.trim();
    const kind = q(".cap-kind").value;
    let body;
    try {
      body = kind === "mcp" ? { kind: "mcp", server: JSON.parse(q(".cap-server").value) }
        : kind === "path" ? { kind: "plugin", path: q(".cap-path").value.trim() }
        : { kind: "plugin", plugin: q(".cap-plugin").value };
    } catch (e) { q(".cap-state").textContent = "The server is not JSON: " + e.message; return; }
    if (await call(`/v1/capabilities/${encodeURIComponent(id)}`, "PUT", body, `Added ${id}.`)) q(".cap-id").value = "";
  }

  function removeEntry(id) {
    if (confirm(`Remove "${id}" from the catalog? The daemon refuses while an agent still lists it.`)) call(`/v1/capabilities/${encodeURIComponent(id)}`, "DELETE", null, `Removed ${id}.`);
  }

  async function importHost() {
    const r = await call("/v1/capabilities/import", "POST", null, "");
    if (!r) return;
    const added = r.added || [];
    q(".cap-state").textContent = (added.length ? "Imported " + added.join(", ") + "." : "Nothing new to import.") + ((r.skipped || []).length ? " Skipped: " + r.skipped.join("; ") : "");
  }

  // call is one catalog request; the reply body on success, else null with
  // the reason in the state line. The catalog reloads after every change.
  async function call(path, method, body, okText) {
    try {
      const init = { method };
      if (body) { init.headers = { "Content-Type": "application/json" }; init.body = JSON.stringify(body); }
      const r = await fetch(path, init);
      if (!r.ok) throw new Error(await r.text());
      const out = await r.json();
      q(".cap-state").textContent = okText;
      await loadCatalog();
      return out;
    } catch (e) {
      q(".cap-state").textContent = e.message;
      return null;
    }
  }

  window.ccmuxAgentCaps = { mount, fill, value };
})();
