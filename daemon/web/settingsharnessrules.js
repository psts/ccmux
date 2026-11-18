// settingsharnessrules.js: per-folder harness rules — which harness a new
// workspace under a folder PRESELECTS on its harness bar (a suggestion,
// nothing auto-starts; longest matching folder wins). A sub-editor of the
// Harnesses tab: settingsharnesses.js owns the settings read and calls
// render() with the current harness names, both on load and after a harness
// save (renames and deletes change what a rule can point at). Loads after
// that file, which made the tab these elements sit in.
(() => {
  "use strict";
  const S = window.ccmuxSettings;
  const rulesBox = $("harness-rules"), statusEl = $("harness-rules-state");
  let harnessNames = [];

  // A rule names a harness, so its select is built from the loaded list; a
  // rule whose harness no longer exists keeps a disabled option so the row
  // stays visible and deletable instead of silently jumping to another name.
  function ruleRow(rule) {
    const row = document.createElement("div");
    row.className = "rule-row";
    const options = harnessNames.map((n) =>
      `<option value="${esc(n)}"${n === rule.harness ? " selected" : ""}>${esc(n)}</option>`);
    if (rule.harness && !harnessNames.includes(rule.harness)) {
      options.push(`<option value="${esc(rule.harness)}" selected disabled>${esc(rule.harness)} (gone)</option>`);
    }
    row.innerHTML =
      `<input class="setting-input rule-prefix" type="text" spellcheck="false" placeholder="/path/to/folder" value="${esc(rule.pathPrefix || "")}">` +
      `<select class="setting-input rule-harness">${options.join("")}</select>` +
      `<button class="rule-del" type="button" title="Remove rule">&times;</button>`;
    const prefix = row.querySelector(".rule-prefix");
    prefix.addEventListener("change", saveRules);
    prefix.addEventListener("keydown", (e) => { if (e.key === "Enter") prefix.blur(); });
    row.querySelector(".rule-harness").addEventListener("change", saveRules);
    row.querySelector(".rule-del").onclick = () => { row.remove(); saveRules(); };
    return row;
  }

  function collect() {
    return [...rulesBox.querySelectorAll(".rule-row")].map((row) => ({
      pathPrefix: row.querySelector(".rule-prefix").value.trim(),
      harness: row.querySelector(".rule-harness").value,
    }));
  }

  function render(names, rules) {
    harnessNames = names;
    rulesBox.innerHTML = "";
    for (const rule of rules) rulesBox.appendChild(ruleRow(rule));
  }

  async function saveRules() {
    try {
      await S.put({ harnessRules: collect() });
      // Don't re-render: half-filled rows stay editable (the daemon drops them).
      statusEl.textContent = "Saved.";
    } catch (e) {
      statusEl.textContent = "Not saved: " + e.message;
    }
  }

  $("harness-rule-add").addEventListener("click", () => {
    rulesBox.appendChild(ruleRow({ pathPrefix: "", harness: harnessNames[0] || "claude" }));
    rulesBox.lastChild.querySelector(".rule-prefix").focus();
  });

  window.ccmuxHarnessRules = { render, collect };
})();
