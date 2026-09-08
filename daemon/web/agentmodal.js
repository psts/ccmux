// Agent editor: one modal for a whole base — identity, role, harness and
// model, permissions, skills, MCP servers, opencode plugins, lifecycle, and
// where it is deployed. Skills come from where they live (a git folder URL
// or dropped files), MCP servers from the snippet a readme ships. Saving
// the form is one PUT; skills and servers act at once and bump the base,
// and the Instances section restarts running copies on request.
(() => {
  let cur = null; // { name, isNew, cfg }
  const modal = () => $("agent-modal");
  const q = (sel) => modal().querySelector(sel);
  const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
  const PERMS = ["read", "edit", "bash", "webfetch"];
  let onChange = () => {};

  function build() {
    const m = modal();
    if (m.dataset.built) return;
    m.dataset.built = "1";
    const sel = (cls, opts) => `<select class="setting-input ${cls}">${opts.map((o) => `<option value="${o}">${o}</option>`).join("")}</select>`;
    m.querySelector(".agent-modal-body").innerHTML =
      section("Identity",
        `<div class="entry-line"><input class="setting-input am-icon" placeholder="⚙" title="icon"><input class="setting-input am-name grow" placeholder="name (a-z, 0-9, -)" spellcheck="false"><span class="am-version agent-version"></span></div>` +
        `<input class="setting-input am-desc" placeholder="One sentence: what it does and when to call it. Other agents read this to decide.">`) +
      section("Role", `<textarea class="setting-input am-instr" rows="8" spellcheck="false" placeholder="# Role&#10;&#10;What it does, inputs, outputs, how it works, what it never does…"></textarea>`) +
      section("Harness and model",
        `<div class="entry-line"><label class="am-lbl">harness</label>${sel("am-harness", [""])}<label class="am-lbl">account</label><input class="setting-input am-account grow" list="am-accounts" placeholder="default"><datalist id="am-accounts"></datalist></div>` +
        `<div class="entry-line"><label class="am-lbl">model</label><input class="setting-input am-model grow" placeholder="default for the harness, e.g. anthropic/claude-sonnet-5"></div>`) +
      section("Permissions",
        `<div class="entry-line am-perms">` + PERMS.map((p) => `<label class="am-lbl">${p}</label>${sel("am-perm-" + p, ["allow", "ask", "deny"])}`).join("") + `</div>` +
        `<textarea class="setting-input am-bashallow" rows="2" spellcheck="false" placeholder="bash patterns always allowed, one per line, e.g.  git log *"></textarea>`) +
      section("Skills",
        `<div class="am-skills rules-list"></div>` +
        `<div class="entry-line"><input class="setting-input am-skill-url grow" spellcheck="false" placeholder="https://github.com/owner/repo/tree/main/skills/name"><button class="rule-add am-skill-add" type="button">Add from URL</button></div>` +
        `<div class="am-drop">Or drop a SKILL.md or a skill folder here</div>` +
        `<p class="hint am-skills-state"></p>`) +
      section("MCP servers",
        `<div class="am-mcp rules-list"></div>` +
        `<textarea class="setting-input am-mcp-paste" rows="3" spellcheck="false" placeholder='Paste the snippet from the server&#39;s readme: {"mcpServers": {"name": {"command": "...", "args": [...]}}}'></textarea>` +
        `<div class="entry-line"><button class="rule-add am-mcp-add" type="button">Add servers</button><span class="hint am-mcp-state"></span></div>`) +
      section("opencode plugins", `<textarea class="setting-input am-plugins" rows="2" spellcheck="false" placeholder="plugin packages, one per line"></textarea>`) +
      section("Lifecycle",
        `<div class="entry-line"><label class="am-lbl">start</label>${sel("am-start", ["fresh", "continue"])}<label class="am-lbl">idle</label><input class="setting-input agent-num am-idle" type="number" min="0"><span class="am-lbl">min, 0 = never sleeps</span>` +
        `<label class="hx-confirm"><input type="checkbox" class="am-keep"> keep alive</label></div>`) +
      section("Instances", `<div class="am-instances rules-list"></div><div class="entry-line"><button class="rule-add am-restart" type="button">Restart to apply</button><span class="hint am-inst-state"></span></div>`);
    q("#agent-modal-close").onclick = close;
    m.onclick = (e) => { if (e.target === m) close(); };
    q("#agent-modal-save").onclick = save;
    q(".am-skill-add").onclick = () => addSkillURL();
    q(".am-mcp-add").onclick = () => addMCP();
    q(".am-restart").onclick = () => restart();
    wireDrop(q(".am-drop"));
  }

  function section(title, body) {
    return `<details class="am-section" open><summary>${title}</summary><div class="am-section-body">${body}</div></details>`;
  }

  // open shows the modal for an existing agent (a) or a new one (null).
  async function open(a, cfg, changed) {
    build();
    onChange = changed || (() => {});
    cur = { name: a ? a.name : "", isNew: !a, cfg };
    q("#agent-modal-title").textContent = a ? `⚙ ${a.name}` : "New agent";
    fillOptions(cfg);
    fillForm(a || {});
    for (const s of [".am-skills", ".am-mcp", ".am-instances"]) q(s).innerHTML = "";
    q(".am-skills-state").textContent = a ? "" : "Save the agent first, then add skills.";
    q(".am-mcp-state").textContent = a ? "" : "Save the agent first.";
    q(".am-inst-state").textContent = a ? "" : "Not deployed yet. Add it to a window from the window's ⚙ menu.";
    q("#agent-modal-state").textContent = "";
    modal().classList.remove("hidden");
    (a ? q(".am-desc") : q(".am-name")).focus();
    if (a) await Promise.all([loadSkills(), loadMCP(), loadInstances()]);
  }

  function close() { modal().classList.add("hidden"); cur = null; }

  function fillOptions(cfg) {
    const names = [""].concat((cfg.harnesses || []).map((h) => h.name));
    q(".am-harness").innerHTML = names.map((h) => `<option value="${esc(h)}">${h ? esc(h) : "default harness"}</option>`).join("");
    q("#am-accounts").innerHTML = (cfg.llmAccounts || []).map((a) => `<option value="${esc(a.name)}">`).join("");
  }

  function fillForm(a) {
    const p = a.permissions || {};
    q(".am-icon").value = a.icon || "";
    q(".am-name").value = a.name || "";
    q(".am-name").readOnly = !!a.name;
    q(".am-version").textContent = a.version ? "v" + a.version : "";
    q(".am-desc").value = a.description || "";
    q(".am-instr").value = a.instructions || "";
    const hs = q(".am-harness");
    if (a.harness && ![...hs.options].some((o) => o.value === a.harness)) hs.add(new Option(a.harness, a.harness));
    hs.value = a.harness || "";
    q(".am-account").value = a.account || "";
    q(".am-model").value = a.model || "";
    for (const perm of PERMS) q(".am-perm-" + perm).value = p[perm] || (perm === "read" || perm === "edit" ? "allow" : "ask");
    q(".am-bashallow").value = (p.bashAllow || []).join("\n");
    q(".am-plugins").value = (a.plugins || []).join("\n");
    q(".am-start").value = a.start || "fresh";
    q(".am-idle").value = a.idleExitMinutes ?? 10;
    q(".am-keep").checked = !!a.keepAlive;
  }

  function formValue() {
    const lines = (sel) => q(sel).value.split("\n").map((s) => s.trim()).filter(Boolean);
    const permissions = {};
    for (const perm of PERMS) permissions[perm] = q(".am-perm-" + perm).value;
    permissions.bashAllow = lines(".am-bashallow");
    return {
      icon: q(".am-icon").value.trim(), description: q(".am-desc").value.trim(), instructions: q(".am-instr").value,
      harness: q(".am-harness").value, account: q(".am-account").value.trim(), model: q(".am-model").value.trim(),
      permissions, plugins: lines(".am-plugins"), start: q(".am-start").value,
      idleExitMinutes: Number(q(".am-idle").value) || 0, keepAlive: q(".am-keep").checked,
    };
  }

  async function save() {
    const name = q(".am-name").value.trim();
    const v = formValue();
    const state = q("#agent-modal-state");
    if (!name || !v.description) { state.textContent = "An agent needs a name and a one-sentence description."; return; }
    try {
      const r = await fetch(`/v1/agents/${encodeURIComponent(name)}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(v) });
      if (!r.ok) throw new Error(await r.text());
      const saved = await r.json();
      const wasNew = cur.isNew;
      cur.name = saved.name; cur.isNew = false;
      q(".am-name").readOnly = true;
      q(".am-version").textContent = "v" + saved.version;
      q("#agent-modal-title").textContent = `⚙ ${saved.name}`;
      state.textContent = `Saved v${saved.version}.` + (wasNew ? " You can add skills and servers now." : "");
      onChange();
      if (wasNew) { q(".am-skills-state").textContent = ""; q(".am-mcp-state").textContent = ""; }
      await loadInstances();
    } catch (e) {
      state.textContent = "Not saved: " + e.message;
    }
  }

  // --- skills ---
  async function loadSkills() {
    if (!cur || cur.isNew) return;
    const box = q(".am-skills");
    box.innerHTML = "";
    try {
      const r = await fetch(`/v1/agents/${encodeURIComponent(cur.name)}/skills`);
      if (!r.ok) throw new Error(await r.text());
      const list = (await r.json()).skills || [];
      if (!list.length) box.appendChild(el("p", "hint", "No skills yet."));
      for (const sk of list) box.appendChild(skillRow(sk));
    } catch (e) { q(".am-skills-state").textContent = "Couldn't load skills: " + e.message; }
  }

  function skillRow(sk) {
    const row = el("div", "entry-card am-row");
    const line = el("div", "entry-line");
    const text = el("div", "grow");
    text.appendChild(el("div", "am-row-title", sk.name));
    text.appendChild(el("div", "am-row-sub", sk.description || ""));
    if (sk.source) text.appendChild(el("div", "am-row-src", sk.source));
    line.appendChild(text);
    if (sk.source) {
      const up = el("button", "rule-add", "Update");
      up.onclick = () => act(`/skills/${encodeURIComponent(sk.name)}/update`, "POST", null, ".am-skills-state", `Updated ${sk.name}.`, loadSkills);
      line.appendChild(up);
    }
    const del = el("button", "rule-del", "×");
    del.title = "Remove skill";
    del.onclick = () => { if (confirm(`Remove skill "${sk.name}"?`)) act(`/skills/${encodeURIComponent(sk.name)}`, "DELETE", null, ".am-skills-state", `Removed ${sk.name}.`, loadSkills); };
    line.appendChild(del);
    row.appendChild(line);
    return row;
  }

  async function addSkillURL() {
    const input = q(".am-skill-url");
    const url = input.value.trim();
    if (!url) return;
    q(".am-skills-state").textContent = "Fetching…";
    // The URL stays in the box on failure, so it can be fixed rather than retyped.
    if (await act("/skills", "POST", { url }, ".am-skills-state", "Installed.", loadSkills)) input.value = "";
  }

  // wireDrop takes a dropped SKILL.md, a set of files, or a folder (walked
  // through the directory entries browsers expose) and sends them as text.
  function wireDrop(zone) {
    zone.ondragover = (e) => { e.preventDefault(); zone.classList.add("over"); };
    zone.ondragleave = () => zone.classList.remove("over");
    zone.ondrop = async (e) => {
      e.preventDefault();
      zone.classList.remove("over");
      if (!cur || cur.isNew) { q(".am-skills-state").textContent = "Save the agent first."; return; }
      const files = {};
      let folder = "";
      try {
        for (const item of e.dataTransfer.items) {
          const entry = item.webkitGetAsEntry && item.webkitGetAsEntry();
          if (entry && entry.isDirectory) { folder = folder || entry.name; await walk(entry, "", files); }
          else if (entry && entry.isFile) { const f = item.getAsFile(); if (f) files[f.name] = await f.text(); }
        }
      } catch (err) {
        q(".am-skills-state").textContent = "Couldn't read the drop: " + err.message;
        return;
      }
      if (!files["SKILL.md"]) { q(".am-skills-state").textContent = "That has no SKILL.md."; return; }
      await act("/skills", "POST", { name: folder || "skill", files }, ".am-skills-state", "Installed.", loadSkills);
    };
  }

  // walk reads a dropped folder into files. readEntries hands back a folder
  // in batches and an empty batch means done, so it is called until then;
  // a read error rejects rather than hanging the drop.
  function walk(dir, prefix, files) {
    const reader = dir.createReader();
    const batch = () => new Promise((res, rej) => reader.readEntries(res, rej));
    return (async () => {
      for (let entries = await batch(); entries.length; entries = await batch()) {
        for (const en of entries) {
          if (en.isDirectory) await walk(en, prefix + en.name + "/", files);
          else files[prefix + en.name] = await new Promise((res, rej) => en.file((f) => f.text().then(res, rej), rej));
        }
      }
    })();
  }

  // --- MCP ---
  async function loadMCP() {
    if (!cur || cur.isNew) return;
    const box = q(".am-mcp");
    box.innerHTML = "";
    try {
      const r = await fetch(`/v1/agents/${encodeURIComponent(cur.name)}/mcp`);
      if (!r.ok) throw new Error(await r.text());
      const list = (await r.json()).servers || [];
      if (!list.length) box.appendChild(el("p", "hint", "No servers yet. The peers bus is always there."));
      for (const sv of list) box.appendChild(mcpRow(sv));
    } catch (e) { q(".am-mcp-state").textContent = "Couldn't load servers: " + e.message; }
  }

  function mcpRow(sv) {
    const row = el("div", "entry-card am-row");
    const line = el("div", "entry-line");
    const text = el("div", "grow");
    text.appendChild(el("div", "am-row-title", sv.name));
    text.appendChild(el("div", "am-row-sub", [sv.command].concat(sv.args || []).join(" ")));
    const envs = Object.keys(sv.env || {});
    if (envs.length) text.appendChild(el("div", "am-row-src", "env: " + envs.join(", ")));
    line.appendChild(text);
    const del = el("button", "rule-del", "×");
    del.title = "Remove server";
    del.onclick = () => { if (confirm(`Remove MCP server "${sv.name}"?`)) act(`/mcp/${encodeURIComponent(sv.name)}`, "DELETE", null, ".am-mcp-state", `Removed ${sv.name}.`, loadMCP); };
    line.appendChild(del);
    row.appendChild(line);
    return row;
  }

  async function addMCP() {
    const ta = q(".am-mcp-paste");
    const text = ta.value.trim();
    if (!text) return;
    if (await act("/mcp", "POST", text, ".am-mcp-state", "Added.", loadMCP)) ta.value = "";
  }

  // --- instances ---
  async function loadInstances() {
    if (!cur || cur.isNew) return;
    const box = q(".am-instances");
    box.innerHTML = "";
    try {
      const r = await fetch(`/v1/agents/${encodeURIComponent(cur.name)}/instances`);
      if (!r.ok) throw new Error(await r.text());
      const list = (await r.json()).instances || [];
      q(".am-restart").disabled = !list.some((i) => i.state === "running");
      if (!list.length) { box.appendChild(el("p", "hint", "Not deployed to any window yet. Add it from a window's ⚙ menu.")); return; }
      for (const i of list) {
        const row = el("div", "entry-card am-row");
        const line = el("div", "entry-line");
        line.appendChild(el("span", "grow", `${i.window} · ${i.state}`));
        line.appendChild(el("span", "am-row-sub", i.paneVersion ? `runs v${i.paneVersion}` + (i.drift ? " ↻ older than the base" : "") : ""));
        const n = i.schedules || 0;
        line.appendChild(el("span", "am-row-sub", n ? ` · ${n} schedule${n === 1 ? "" : "s"}` : ""));
        row.appendChild(line);
        box.appendChild(row);
      }
    } catch (e) { q(".am-inst-state").textContent = "Couldn't load instances: " + e.message; }
  }

  async function restart() {
    await act("/instances/restart", "POST", null, ".am-inst-state", "Restarting running instances; they pick up the new base as they come back.", () => setTimeout(loadInstances, 3000));
  }

  // act is one call against the agent's sub-resources, reporting into a
  // state line and refreshing a list on success; true when it went through.
  async function act(path, method, body, stateSel, okText, reload) {
    if (!cur) return false;
    const state = q(stateSel);
    try {
      const init = { method };
      if (body != null) {
        init.headers = { "Content-Type": "application/json" };
        init.body = typeof body === "string" ? body : JSON.stringify(body);
      }
      const r = await fetch(`/v1/agents/${encodeURIComponent(cur.name)}${path}`, init);
      if (!r.ok) throw new Error(await r.text());
      state.textContent = okText;
      onChange();
      await reload();
      refreshVersion();
      return true;
    } catch (e) {
      state.textContent = e.message;
      return false;
    }
  }

  // Every skill or server change bumps the base; show the new version.
  async function refreshVersion() {
    if (!cur) return;
    try {
      const list = (await (await fetch("/v1/agents")).json()).agents || [];
      const a = list.find((x) => x.name === cur.name);
      if (a) q(".am-version").textContent = "v" + a.version;
    } catch (_) { /* the next open shows it */ }
  }

  window.ccmuxAgentModal = { open, close };
})();
