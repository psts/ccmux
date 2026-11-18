// settingsharnesses.js: the Settings window's Harnesses tab, the single
// source of what a pane runs. The daemon lists claude (builtin) and every
// known program it finds installed (detected) with zero config; editing one
// of those rows saves a user OVERRIDE by name, and only overrides and new
// entries are persisted — an untouched builtin/detected row stays
// live-resolved (a detected harness disappears when uninstalled). Deleting
// an override restores the default. The command field is where per-harness
// flags live, e.g. claude's --dangerously-load-development-channels.
//
// This file draws the rows and reads them back; the rules are
// settingsharness.js's, and the per-folder rules under the list are
// settingsharnessrules.js's, which loads after this file made the tab.
(() => {
  "use strict";
  const S = window.ccmuxSettings, H = window.ccmuxSettingsHarness;

  S.addTab({
    page: "harnesses", label: "Harnesses", opened: load,
    html:
      `<div id="harness-list" class="rules-list"></div>` +
      `<button id="harness-add" class="rule-add" type="button">+ Add harness</button>` +
      `<p id="harness-state" class="hint setting-hint"></p>` +
      `<label class="setting-label">Per-folder default harness (longest match wins)</label>` +
      `<div id="harness-rules" class="rules-list"></div>` +
      `<button id="harness-rule-add" class="rule-add" type="button">+ Add folder rule</button>` +
      `<p id="harness-rules-state" class="hint setting-hint"></p>`,
  });
  const box = $("harness-list");
  const say = (text) => { $("harness-state").textContent = text; };
  const rules = () => window.ccmuxHarnessRules;
  // The accounts a harness could use, newest load wins. Needed here because
  // the per-harness order names accounts, and the list it offers must match
  // what the Accounts tab holds.
  let accounts = [];

  const storedOrder = (row) => JSON.parse(row.dataset.order || "[]");
  const isCustom = (row) => row.querySelector(".hx-order-pick input[value=custom]").checked;
  const checkedKinds = (row) => [...row.querySelectorAll(".hx-kinds input:checked")].map((el) => el.dataset.kind);

  function kindChecks(h) {
    return H.KINDS.map((k) =>
      `<label class="hx-confirm"><input type="checkbox" data-kind="${k}"${(h.accountKinds || []).includes(k) ? " checked" : ""}>${k}</label>`).join("");
  }

  // Radio groups are keyed by name, so each row needs its own (radioName) or
  // checking one harness's radio would clear every other row's.
  function orderPick(custom, radioName) {
    return `<div class="entry-line hx-order-pick">` +
      `<span class="hx-kinds-label">order:</span>` +
      `<label class="hx-confirm"><input type="radio" name="${esc(radioName)}" value="global"${custom ? "" : " checked"}>follow the account order</label>` +
      `<label class="hx-confirm"><input type="radio" name="${esc(radioName)}" value="custom"${custom ? " checked" : ""}>custom for this harness</label>` +
      `</div>`;
  }

  function rowHTML(h, custom) {
    const badge = h.source ? `<span class="harness-src">${esc(h.source)}</span>` : "";
    return `<div class="entry-line">` +
      `<input class="setting-input hx-icon" type="text" spellcheck="false" placeholder="✳" value="${esc(h.icon || "")}">` +
      `<input class="setting-input hx-name grow" type="text" spellcheck="false" placeholder="name" value="${esc(h.name || "")}">` +
      badge +
      `<label class="hx-confirm" title="Press Enter through its startup prompts"><input type="checkbox" ${h.autoconfirm ? "checked" : ""}>auto-ok</label>` +
      (h.source ? "" : `<button class="rule-del" type="button" title="Remove harness">&times;</button>`) +
      `</div>` +
      `<div class="entry-line">` +
      `<input class="setting-input hx-cmd grow" type="text" spellcheck="false" placeholder="command + flags" value="${esc(h.command || "")}">` +
      `</div>` +
      `<div class="entry-line hx-kinds" title="Which llm account kinds this harness can use; none checked = its default">` +
      `<span class="hx-kinds-label">accounts:</span>` +
      kindChecks(h) +
      `</div>` +
      `<div class="entry-line hx-dialect hint"></div>` +
      orderPick(custom, `hx-order-${Math.random().toString(36).slice(2)}`) +
      `<div class="entry-line hx-order"></div>`;
  }

  function harnessRow(h, keepCustom) {
    const row = document.createElement("div");
    row.className = "entry-card";
    row.dataset.orig = H.snapshot(h);
    row.dataset.source = h.source || "";
    row.innerHTML = rowHTML(h, !!(h.accountOrder || []).length || !!keepCustom);
    // Checking a kind changes which accounts the order may hold, and the
    // radio decides whether the list shows at all. Both must redraw BEFORE
    // the save: rowValue reads the order back off the row, so saving first
    // would send the list as it was — for the radio that means turning
    // "custom" on and persisting no order at all, and the choice silently
    // reverting on the next load.
    for (const el of row.querySelectorAll(".hx-kinds input, .hx-order-pick input")) {
      el.addEventListener("change", () => renderOrder(row));
    }
    for (const el of row.querySelectorAll("input")) {
      el.addEventListener("change", save);
      el.addEventListener("keydown", (e) => { if (e.key === "Enter") el.blur(); });
    }
    const del = row.querySelector(".rule-del");
    if (del) del.onclick = () => { row.remove(); save(); };
    row.dataset.order = JSON.stringify(h.accountOrder || []);
    renderOrder(row);
    return row;
  }

  function orderItems(ordered) {
    return ordered.map((a, i) =>
      `<span class="hx-order-item" data-name="${esc(a.name)}">` +
      `${i + 1}. ${esc(a.name)} <span class="llm-kind">${esc(a.kind)}</span>` +
      `<button class="ord-up" type="button" title="Try this account earlier">▲</button>` +
      `<button class="ord-dn" type="button" title="Try this account later">▼</button>` +
      `</span>`).join("");
  }

  // renderOrder redraws one row's account list from its checked kinds and
  // its stored order. Held in row.dataset rather than read back off the DOM
  // so a kind the user unchecks and rechecks does not lose its position.
  function renderOrder(row) {
    const listEl = row.querySelector(".hx-order");
    const kinds = checkedKinds(row);
    row.querySelector(".hx-dialect").textContent = H.dialectWarning(kinds);
    if (!isCustom(row)) {
      listEl.innerHTML = "";
      return;
    }
    const ordered = H.orderedFor(accounts, kinds, storedOrder(row));
    row.dataset.order = JSON.stringify(H.seeded(storedOrder(row), ordered));
    if (!ordered.length) {
      // The radio stays on custom and says why. An empty custom order still
      // persists as no override, because it resolves identically to
      // following the account order — same rule as the Mac lens.
      listEl.innerHTML = `<span class="hint">no account matches the kinds above</span>`;
      return;
    }
    listEl.innerHTML = orderItems(ordered);
    const names = ordered.map((a) => a.name);
    listEl.querySelectorAll(".hx-order-item").forEach((item, i) => {
      item.querySelector(".ord-up").onclick = () => moveInOrder(row, names, i, -1);
      item.querySelector(".ord-dn").onclick = () => moveInOrder(row, names, i, 1);
    });
  }

  function moveInOrder(row, names, i, delta) {
    const next = H.swapped(storedOrder(row), names, i, delta);
    if (!next) return;
    row.dataset.order = JSON.stringify(next);
    renderOrder(row);
    save();
  }

  function rowValue(row) {
    const kinds = checkedKinds(row);
    const v = {
      icon: row.querySelector(".hx-icon").value.trim(),
      name: row.querySelector(".hx-name").value.trim(),
      command: row.querySelector(".hx-cmd").value.trim(),
      autoconfirm: row.querySelector(".hx-confirm input").checked,
    };
    if (kinds.length) v.accountKinds = kinds;
    // Only a custom order is sent: "follow the account order" is the absence
    // of the field, which is what makes the global order keep reaching this
    // harness as accounts are added and moved.
    //
    // From the row's stored order, not from the painted list: the dataset
    // is the single record renderOrder and moveInOrder both write, so a
    // save cannot depend on whether the DOM has been redrawn yet.
    if (isCustom(row) && storedOrder(row).length) v.accountOrder = storedOrder(row);
    return v;
  }

  function collect() {
    const out = [];
    for (const row of box.querySelectorAll(".entry-card")) {
      const v = rowValue(row);
      if (!v.name && !v.command) continue; // blank editor row
      const untouchedDefault = row.dataset.source && H.snapshot(v) === row.dataset.orig;
      if (untouchedDefault) continue; // stays live-resolved, not frozen
      out.push(v);
    }
    return out;
  }

  // Which rows had the custom-order radio on, and had NOT yet been given
  // an order to store. Rebuilding the radio from accountOrder being
  // non-empty is the derivation the field exists to avoid: a harness
  // whose kinds match no account stores an empty order, so the radio
  // snapped back the moment the user saved.
  //
  // Only rows whose stored order is still empty carry over. Keeping it
  // for every checked row meant a stale tab resurrected a custom order
  // another lens had just turned OFF, and the next save wrote it back.
  function customWithoutOrder() {
    return new Set([...box.querySelectorAll(".entry-card")]
      .filter((r) => isCustom(r))
      .filter((r) => !storedOrder(r).length)
      .map((r) => r.querySelector(".hx-name").value.trim()));
  }

  async function load() {
    try {
      const wasCustom = customWithoutOrder();
      const cfg = await S.read(); // unreadable is not "no harnesses", and the reason travels with it
      accounts = cfg.llmAccounts || []; // before the rows: they render from it
      box.innerHTML = "";
      for (const h of cfg.harnesses || []) box.appendChild(harnessRow(h, wasCustom.has(h.name)));
      rules().render((cfg.harnesses || []).map((h) => h.name), cfg.harnessRules || []);
      say("Installed harnesses appear on their own; edit a row to override it. A harness tries its accounts top-down and falls through when one hits its limit.");
    } catch (e) {
      console.error("harness settings load failed", e);
      say("Couldn't load harnesses: " + e.message);
    }
  }

  async function save() {
    try {
      const cfg = await S.put({ harnesses: collect() });
      // Renames/deletes change what a rule can point at — rebuild the selects.
      rules().render((cfg.harnesses || []).map((h) => h.name), rules().collect());
      say("Saved.");
    } catch (e) {
      say("Not saved: " + e.message);
    }
  }

  $("harness-add").addEventListener("click", () => {
    box.appendChild(harnessRow({}, false));
    box.lastChild.querySelector(".hx-name").focus();
  });
})();
