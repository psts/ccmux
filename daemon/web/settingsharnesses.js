// settingsharnesses.js: the Settings window's Harnesses tab.
(() => {
  "use strict";
  window.ccmuxSettings.addTab({
    page: "harnesses", label: "Harnesses",
    html:
      `<div id="harness-list" class="rules-list"></div>` +
      `<button id="harness-add" class="rule-add" type="button">+ Add harness</button>` +
      `<p id="harness-state" class="hint setting-hint"></p>` +
      `<label class="setting-label">Per-folder default harness (longest match wins)</label>` +
      `<div id="harness-rules" class="rules-list"></div>` +
      `<button id="harness-rule-add" class="rule-add" type="button">+ Add folder rule</button>` +
      `<p id="harness-rules-state" class="hint setting-hint"></p>`,
  });
})();
