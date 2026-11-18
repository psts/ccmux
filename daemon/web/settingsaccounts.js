// settingsaccounts.js: the Settings window's Accounts tab, LLM routing.
// Accounts are places pane LLM traffic can go (Ollama, OpenRouter, a keyed
// Anthropic org); the route picks which one answers right now. Empty route =
// direct Anthropic pass-through (the Max OAuth default).
//
// The list is a SUMMARY: what each account is, how it is doing, and where
// it sits in the failover order. Everything you set rather than read lives
// in the editor (settingsllmedit.js), because this tab is read far more
// often than it is edited and the order arrows are the thing you reach for.
// This file owns the account list and the one queue every change to it goes
// through; what the words and the changes mean is settingsllm.js's.
(() => {
  "use strict";
  const S = window.ccmuxSettings, L = window.ccmuxSettingsLLM;

  S.addTab({
    page: "accounts", label: "Accounts", opened: load,
    html:
      `<label class="setting-label" for="llm-route">Default account (panes ccmux did not start)</label>` +
      `<select id="llm-route" class="setting-input"></select>` +
      `<p id="llm-route-state" class="hint setting-hint"></p>` +
      `<label class="setting-label">LLM accounts (top first; a harness skips the kinds it cannot use)</label>` +
      `<div id="llm-accounts" class="rules-list"></div>` +
      `<button id="llm-account-add" class="rule-add" type="button">+ Add account</button>` +
      `<p id="llm-state" class="hint setting-hint"></p>`,
  });
  const routeSel = $("llm-route"), box = $("llm-accounts"), routeStateEl = $("llm-route-state");
  const say = (text) => { $("llm-state").textContent = text; };

  // `accounts` is the record the rows render from, not the DOM. Reading edits
  // back out of input fields is what made every keystroke PUT the whole list.
  let accounts = [];
  let statuses = {};
  // Accounts with a Check in flight. Kept here, not on the button, because
  // any redraw rebuilds every row: a button-only flag came back enabled and
  // let a second probe of the same account start mid-check.
  const checking = new Set();
  let sidecars = {};

  const health = (a) => L.healthLine(a, statuses[a.name], sidecars[a.name]);

  function checkButton(a) {
    if (!L.checkable(statuses[a.name])) return "";
    const busy = checking.has(a.name);
    return `<button class="rule-add llm-check" type="button" title="Ask Anthropic for this account's usage now. A limit that lifted early (a manual reset) frees the account at once."${busy ? " disabled" : ""}>${busy ? "Checking…" : "Check"}</button>`;
  }

  function accountRow(a, i) {
    const row = document.createElement("div");
    row.className = "entry-card";
    const status = health(a);
    row.innerHTML =
      `<div class="entry-line">` +
      `<span class="llm-pos">${i + 1}</span>` +
      `<span class="llm-acct-name grow">${esc(a.name || "(unnamed)")}</span>` +
      `<span class="llm-kind">${esc(a.kind || "anthropic")}</span>` +
      `<button class="ord-up" type="button" title="Try this account earlier">▲</button>` +
      `<button class="ord-dn" type="button" title="Try this account later">▼</button>` +
      checkButton(a) +
      `<button class="rule-add llm-edit" type="button">Edit</button>` +
      `<button class="rule-del" type="button" title="Remove account">&times;</button>` +
      `</div>` +
      (status ? `<div class="entry-line llm-acct-status">${esc(status)}</div>` : "");
    const checkBtn = row.querySelector(".llm-check");
    if (checkBtn) checkBtn.onclick = () => checkAccount(a.name);
    row.querySelector(".llm-edit").onclick = () => openAccount(i);
    row.querySelector(".ord-up").onclick = () => moveAccount(i, -1);
    row.querySelector(".ord-dn").onclick = () => moveAccount(i, 1);
    row.querySelector(".rule-del").onclick = () => removeAccount(a.name);
    return row;
  }

  function removeAccount(name) {
    if (!confirm(`Remove account "${name}"? Its stored key goes with it.`)) return;
    // No llmRoute is sent: the DAEMON prunes the default route when the
    // account it names is removed. Deciding that here meant reading this
    // tab's copy of the field, which could be stale enough to wipe a route
    // another lens had just set.
    queueSave((list) => L.removed(list, name))
      .catch((e) => { say("Not removed: " + e.message); });
  }

  // "Check" asks the daemon to probe one account upstream now. A served answer
  // clears a stale limit in the proxy itself, so routing frees the account on
  // the next request; the reply is that account's fresh row, swapped in alone
  // so nothing else on the tab is re-read. A reply that was not about quota
  // records nothing daemon-side and is shown as its reason.
  async function checkAccount(name) {
    if (checking.has(name)) return;
    checking.add(name);
    renderAccounts();
    try {
      const r = await fetch(`/v1/llm/accounts/${encodeURIComponent(name)}/check`, { method: "POST" });
      const answer = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(answer.error || `HTTP ${r.status}`);
      statuses[name] = answer;
      say(`Checked ${name}: ${L.statusLine(answer)}`);
    } catch (e) {
      say(`Check of ${name} failed: ${e.message || e}`);
    } finally {
      checking.delete(name);
    }
    renderAccounts();
  }

  // Row actions run through queueSave and address accounts by NAME rather
  // than by the index the row was drawn with. Both matter together: a save is
  // a round trip, and a second click during one would otherwise build its
  // change from the pre-save list — deleting two accounts in quick succession
  // re-added the first, keyless, because the second request still carried it
  // and the daemon inherits a key only from what it currently stores.
  let saving = Promise.resolve();

  // A mutate returning null CANCELS: nothing is sent and nothing is claimed.
  // Without it, ▲ on the first row PUT the unchanged list and printed
  // "Saved." — a daemon write and a false confirmation for a click that did
  // nothing.
  //
  // A cancelled save resolves with CANCELLED, not undefined, so an awaiting
  // caller can tell it apart from one that ran. The editor closes its sheet on
  // that resolution, and closing it for an edit that was never sent would be
  // the same false success one layer up.
  const CANCELLED = Symbol("cancelled");

  // The daemon's settings as they are now, for a save to start from. A
  // failed read refuses the save rather than falling back to the stale
  // list: unreadable is not empty, the same rule the daemon applies.
  async function currentSettings() {
    const r = await fetch("/v1/settings");
    if (!r.ok) {
      throw new Error(`settings unreadable (HTTP ${r.status}) — nothing saved`);
    }
    const cfg = await r.json();
    // Unreadable is NOT empty. A 503 carries a JSON {"error":…} body, so
    // parsing it succeeds and llmAccounts is simply absent — treating that
    // as an empty list made the next click PUT [] and wipe every account
    // and its stored key. The daemon refuses to serve an empty list for
    // exactly this reason; the client has to refuse to send one.
    if (!Array.isArray(cfg.llmAccounts)) {
      throw new Error("settings carried no account list — nothing saved");
    }
    return cfg;
  }

  // The mutate runs against the DAEMON's current list, re-read here, not
  // against this tab's cached copy.
  //
  // The cached copy is only as fresh as this tab's last save, and every save
  // writes the WHOLE list — so a row action in a stale tab silently undid a
  // delete made anywhere else, and the resurrected account came back with no
  // key, because the daemon inherits a stored key by name and that name was
  // gone. Measured: deleting "doomed" from another client, then clicking ▲ in
  // this tab, put ["doomed","keep"] back.
  async function saveOnFresh(mutate) {
    const cfg = await currentSettings();
    accounts = cfg.llmAccounts.map((a) => ({ ...a }));
    let candidate;
    try {
      candidate = mutate(accounts.slice());
    } catch (e) {
      // The re-read is why the mutate could refuse, so show what it read:
      // leaving the old rows up would keep a row for an account the daemon
      // no longer has, right next to a message saying it is gone.
      redraw(cfg);
      throw e;
    }
    if (!candidate) {
      redraw(cfg); // the re-read may have moved things under the rows
      return CANCELLED;
    }
    try {
      return await saveAccounts(candidate);
    } catch (e) {
      // The PUT was refused, so the daemon still holds what the re-read
      // above returned and `accounts` already matches it — only the drawn
      // rows are behind, and they are addressed by INDEX, so leaving them
      // up made the next ▲ move a different account than the one clicked.
      // Redrawn from cfg rather than reloaded: a reload writes its own
      // closing status line, which is what used to wipe the refusal one
      // fetch later and leave the click reading as a silent no-op.
      redraw(cfg);
      throw e;
    }
  }

  // One change at a time, each built on the one before it.
  function queueSave(mutate) {
    const next = saving.catch(() => {}).then(() => saveOnFresh(mutate));
    saving = next.catch(() => {}); // a refusal must not wedge the queue
    return next;
  }

  function moveAccount(i, delta) {
    const name = (accounts[i] || {}).name;
    // The rejection has to be caught HERE. queueSave's own handler exists
    // only to keep the queue moving, so an uncaught throw was swallowed and
    // the click still read as the previous save's "Saved." — the very defect
    // the throw was added to remove.
    queueSave((list) => L.moved(list, name, delta))
      .catch((e) => { say("Not moved: " + e.message); });
  }

  // The status rows from a settings read or a save's echo. Both carry them
  // (the PUT answers with the GET body), and the Check button is decided from
  // them, so a path that re-seeds `accounts` without these left a newly saved
  // claude account with no button and a former one with a button the daemon
  // then refused. An answer without the field leaves the last rows standing.
  function seedStatuses(cfg) {
    if (!Array.isArray(cfg.llmAccountStatus)) return;
    statuses = {};
    for (const st of cfg.llmAccountStatus) statuses[st.name] = st;
  }

  // Rows and route select from one settings answer.
  function redraw(cfg) {
    seedStatuses(cfg);
    renderAccounts();
    renderRoute(accounts, cfg.llmRoute);
  }

  function renderAccounts() {
    box.innerHTML = "";
    accounts.forEach((a, i) => box.appendChild(accountRow(a, i)));
  }

  function openAccount(i) {
    const a = accounts[i] || {};
    window.ccmuxLLMEditor.open(a, { health: health(a), queueSave, CANCELLED, say });
  }

  function renderRoute(list, route) {
    routeSel.innerHTML = "";
    const direct = document.createElement("option");
    direct.value = ""; direct.textContent = "Anthropic (direct, your Claude login)";
    routeSel.appendChild(direct);
    for (const a of list) {
      if (!a.name) continue;
      const o = document.createElement("option");
      o.value = a.name;
      o.textContent = a.name + (a.baseURL ? "  →  " + a.baseURL : "");
      routeSel.appendChild(o);
    }
    routeSel.value = route || "";
  }

  async function load() {
    try {
      const cfg = await S.read(); // an error answer is not an empty account list
      statuses = {};
      seedStatuses(cfg);
      sidecars = cfg.llmSidecars || {};
      accounts = (cfg.llmAccounts || []).map((a) => ({ ...a }));
      renderAccounts();
      renderRoute(accounts, cfg.llmRoute);
      say("Order decides who answers first; a limited account drops to the back on its own. Applies to every pane's next request — no restarts.");
    } catch (e) {
      // Mirrored to the console because the daemon does not log a refused
      // settings read at all: writeError answers JSON and returns. The
      // harness load does the same for the same reason.
      console.error("LLM settings load failed", e);
      say("Couldn't load LLM settings: " + e.message);
    }
  }

  // saveAccounts takes the list to PUT rather than reading the module one,
  // and `accounts` only advances when the daemon accepted it. Mutating first
  // and saving second is what let a REFUSED edit sit in memory until the next
  // unrelated row click PUT it — silently, and without the key it never
  // carried, so the stored credential went with it.
  //
  // A refusal is never caught here. Every caller already has a catch that
  // names what the user just tried ("Not removed:", "Not moved:", the sheet's
  // own line) and swallowing it here made all three dead code: queueSave
  // resolved normally, so a refused delete printed nothing at all.
  async function saveAccounts(candidate) {
    const cfg = await S.put({ llmAccounts: candidate.map(({ apiKeySet, ...rest }) => rest) });
    // Re-seed from the daemon's echo: it normalizes URLs and reports which
    // accounts hold a key, neither of which this side should guess at.
    accounts = (cfg.llmAccounts || []).map((a) => ({ ...a }));
    seedStatuses(cfg);
    renderAccounts();
    renderRoute(accounts, cfg.llmRoute);
    say("Saved.");
  }

  routeSel.addEventListener("change", async () => {
    try {
      await S.put({ llmRoute: routeSel.value });
      routeStateEl.textContent = routeSel.value
        ? `Panes with no harness of their own use ${routeSel.value}.`
        : "Panes with no harness of their own go direct to Anthropic.";
    } catch (e) {
      routeStateEl.textContent = "Not saved: " + e.message;
      load(); // the picker now lies — reload truth
    }
  });
  $("llm-account-add").addEventListener("click", () => openAccount(accounts.length));
})();
