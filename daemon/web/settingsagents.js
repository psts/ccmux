// settingsagents.js: the Settings window's Agents tab.
(() => {
  "use strict";
  window.ccmuxSettings.addTab({
    page: "agents", label: "Agents",
    html:
      `<p class="hint setting-hint">Role agents live as folders under <code>~/.ccmux/agents</code>. Edit one to set its role, skills and tools; add it to a project from the window's ⚙ menu.</p>` +
      `<div id="agent-list" class="rules-list"></div>` +
      `<button id="agent-add" class="rule-add" type="button">+ New agent</button>` +
      `<p id="agent-state" class="hint setting-hint"></p>` +
      `<p id="agent-sdk" class="hint setting-hint"></p>`,
  });
})();
