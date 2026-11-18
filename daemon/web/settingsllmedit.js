// settingsllmedit.js: the one-account editor that opens above Settings from
// the Accounts tab (settingsaccounts.js). Everything you set rather than
// read lives here: URL, key, aliases, the model map. It holds no copy of
// the account list: the tab hands it the account and its own save queue
// (host), and a save goes through that queue. Keys are write-only: an
// account saved with an empty key keeps its stored one, so re-saving this
// form never wipes a secret. Loads after settingsmodal.js and settingsllm.js.
(() => {
  "use strict";
  const S = window.ccmuxSettings, L = window.ccmuxSettingsLLM;
  const modal = S.addEditor("llm", "llm-sheet", "Account");
  const mq = (sel) => modal.querySelector(sel);
  const say = (text) => { $("llm-modal-state").textContent = text; };

  // The tab that opened the sheet: { health, queueSave, CANCELLED, say }.
  let host = null;
  // The NAME the sheet was opened on, and the ONLY way it addresses its
  // account. An index was not enough: a queued delete landing mid-edit
  // re-seeds and re-renders the list, so the index pointed at a different
  // account and Save overwrote THAT one, orphaning its stored key, while the
  // account being edited went untouched. "" means a new account.
  let editingName = "";
  let savingModal = false;
  // Bumped on every open. fillModelPick captures it and drops a response
  // that arrives after the sheet moved on: the modal is ONE element reused
  // for every account, so a slow upstream would otherwise append its models
  // to whichever account you opened next, and picking one would alias that
  // account to a model its own upstream has never heard of.
  let openGeneration = 0;

  const body =
    `<div class="entry-line">` +
    `<input class="setting-input lm-name grow" type="text" spellcheck="false" placeholder="name">` +
    `<select class="setting-input lm-kind">` +
    L.KINDS.map((k) => `<option value="${k}">${k}</option>`).join("") +
    `</select></div>` +
    `<div class="entry-line"><input class="setting-input lm-url grow" type="text" spellcheck="false" placeholder="base URL"></div>` +
    `<div class="entry-line"><input class="setting-input lm-key grow" type="password" autocomplete="off" placeholder="token / api key">` +
    `<button class="lm-key-show" type="button" title="Fetch the stored key from the daemon. Only a verified tailnet login or this machine's owner may.">Show</button></div>` +
    `<div class="entry-line lm-key-shown hidden"><code class="lm-key-value grow"></code>` +
    `<button class="lm-key-copy" type="button">Copy</button></div>` +
    `<div class="entry-line">` +
    `<input class="setting-input lm-aliases grow" type="text" spellcheck="false" placeholder="aliases: claude-haiku-*=qwen3-4b-32k">` +
    `<select class="setting-input lm-model-pick" title="List the upstream's models; picking one maps every claude-* request to it">` +
    `<option value="">map claude → …</option></select>` +
    `</div>` +
    `<p class="hint lm-status"></p>`;

  function build() {
    if (modal.dataset.built) return;
    modal.dataset.built = "1";
    modal.querySelector(".llm-modal-body").innerHTML = body;
    $("llm-modal-close").onclick = close;
    modal.onclick = (e) => { if (e.target === modal) close(); };
    $("llm-modal-save").onclick = save;
    mq(".lm-model-pick").addEventListener("change", (e) => {
      if (!e.target.value) return;
      const field = mq(".lm-aliases");
      field.value = L.aliasesWithModel(field.value, e.target.value);
    });
    mq(".lm-kind").addEventListener("change", () => {
      mq(".lm-key").placeholder = "token / api key: " + L.keyHintFor({ kind: mq(".lm-kind").value });
    });
    mq(".lm-key-show").addEventListener("click", revealKey);
    mq(".lm-key-copy").addEventListener("click", () => copyToClipboard(mq(".lm-key-value").textContent, $("llm-modal-state")));
  }

  // The stored key, fetched on demand and shown beside the box rather than
  // typed into it: a value in the box would be re-sent as a new key on Save,
  // and the box's "empty keeps it" rule is what makes the modal safe to
  // round-trip. The daemon answers only a caller it can vouch for (a
  // verified tailnet login, or the owner on loopback), so a refusal is shown
  // as its reason rather than as a blank.
  async function revealKey() {
    const name = editingName, gen = openGeneration;
    if (!name) return;
    say("Fetching key…");
    try {
      const r = await fetch(`/v1/llm/accounts/${encodeURIComponent(name)}/key`);
      const answer = await r.json().catch(() => ({}));
      if (gen !== openGeneration) return; // the sheet moved on to another account
      if (!r.ok) { say(answer.error || `HTTP ${r.status}`); return; }
      mq(".lm-key-value").textContent = answer.apiKey || "";
      mq(".lm-key-shown").classList.remove("hidden");
      say("");
    } catch (e) {
      if (gen !== openGeneration) return;
      say("couldn't fetch key: " + (e.message || e));
    }
  }

  // fill puts account a into the form; an empty a is a new account.
  function fill(a) {
    mq(".lm-name").value = a.name || "";
    // Read-only once the account exists, like the agent editor's name field.
    // A rename sends the new name with an empty key, and the daemon inherits
    // a stored key BY NAME — so the account silently came back keyless, which
    // for a localhost or api.anthropic.com base URL turns it into a
    // pass-through forwarding each pane's own login. Delete and re-add is the
    // honest way to change a name.
    mq(".lm-name").readOnly = !!a.name;
    mq(".lm-name").title = a.name ? "Names are fixed; delete and re-add to rename" : "";
    mq(".lm-kind").value = a.kind || "anthropic";
    mq(".lm-url").value = a.baseURL || "";
    mq(".lm-url").placeholder = a.kind === "meridian"
      ? "base URL (empty = http://127.0.0.1:3456)" : "base URL, e.g. http://localhost:11434";
    mq(".lm-key").value = "";
    mq(".lm-key").placeholder = "token / api key: " + L.keyHintFor(a);
    // Show only when there is something stored to show: a new account or a
    // pass-through has no key, and the daemon would just say so.
    mq(".lm-key-show").classList.toggle("hidden", !(a.name && a.apiKeySet));
    mq(".lm-key-shown").classList.add("hidden");
    mq(".lm-key-value").textContent = "";
    mq(".lm-aliases").value = L.aliasText(a.modelAliases);
  }

  // open shows the sheet on account a ({} for a new one) for the tab h.
  function open(a, h) {
    build();
    host = h;
    editingName = a.name || "";
    openGeneration++;
    // Reset the in-flight guard: the modal is ONE reused element, so closing
    // it mid-save (backdrop, ✕) would otherwise carry a disabled Save button
    // into the next account, where the guard returns before writing any
    // message and the button just stops working.
    savingModal = false;
    $("llm-modal-save").disabled = false;
    fill(a);
    mq(".lm-status").textContent = h.health;
    $("llm-modal-title").textContent = a.name ? "Account · " + a.name : "New account";
    say("");
    modal.classList.remove("hidden");
    // One upstream, asked when you open it — the list used to ask every
    // account's upstream on every settings open.
    fillModelPick(a, openGeneration);
    mq(".lm-name").focus();
  }

  function close() { modal.classList.add("hidden"); editingName = ""; }

  // The upstream's models, or why it would not list them.
  async function modelsOf(name) {
    const r = await fetch(`/v1/llm/accounts/${encodeURIComponent(name)}/models`);
    if (!r.ok) return { error: (await r.json().catch(() => ({}))).error || `HTTP ${r.status}` };
    return { list: (await r.json()).models || [] };
  }

  // Fill the model picker from this account's upstream. An upstream that
  // doesn't answer says why rather than leaving an empty picker that reads
  // as a broken feature.
  async function fillModelPick(a, generation) {
    const sel = mq(".lm-model-pick");
    const current = L.mappedModel(a);
    sel.innerHTML = `<option value="">map claude → …</option>` +
      (current ? `<option value="${esc(current)}" selected>${esc(current)}</option>` : "");
    if (!a.name) return; // unsaved: there is no upstream to ask yet
    const got = await modelsOf(a.name).catch(() => ({ failed: true }));
    // Checked after the answer OR the rejection: a slow one of either kind
    // would otherwise land on the next account's own list.
    if (generation !== openGeneration) return; // the sheet moved on
    if (got.failed || got.error) {
      if (got.error) sel.title = "couldn't list models: " + got.error;
      sel.options[0].textContent = "models unavailable";
      return;
    }
    for (const m of got.list) {
      if (m === current) continue;
      const o = document.createElement("option");
      o.value = m; o.textContent = m;
      sel.appendChild(o);
    }
  }

  // What the form holds, as the account to store.
  function formValue() {
    const next = {
      name: mq(".lm-name").value.trim(),
      kind: mq(".lm-kind").value,
      baseURL: mq(".lm-url").value.trim(),
      modelAliases: L.parseAliases(mq(".lm-aliases").value),
    };
    // Write-only key: an empty box KEEPS the stored one, which is what lets
    // this form round-trip a redacted account without wiping its secret.
    const key = mq(".lm-key").value;
    if (key) next.apiKey = key;
    return next;
  }

  async function save() {
    const next = formValue();
    if (!next.name) { say("An account needs a name."); return; }
    if (savingModal) { say("Still saving…"); return; } // a second click would push a second copy
    // The sheet this save belongs to. Closing it mid-save and opening another
    // would otherwise let this save close THAT sheet, discarding a
    // half-typed account, or write this save's error into its state line.
    const gen = openGeneration, wasNamed = editingName, tab = host;
    savingModal = true;
    $("llm-modal-save").disabled = true;
    try {
      const result = await tab.queueSave((list) => L.upserted(list, wasNamed, next));
      if (gen !== openGeneration) return; // the sheet moved on
      // The sheet stays open rather than claiming a save happened.
      if (result === tab.CANCELLED) { say("Nothing to save."); return; }
      close();
    } catch (e) {
      // The queue has already redrawn the rows from its own re-read, so what
      // is behind the sheet is the daemon's list; the sheet stays open on the
      // edit that was refused.
      //
      // The TAB's line is written unconditionally, the sheet's only while it
      // is still the same sheet. Writing only into the sheet loses the
      // refusal twice over: closing it does not bump the generation, so the
      // message landed in a hidden element, and opening another account
      // before the PUT settled put it nowhere at all. Either way the edit
      // was dropped in silence.
      tab.say("Not saved: " + e.message);
      if (gen === openGeneration) say("Not saved: " + e.message);
    } finally {
      savingModal = false;
      $("llm-modal-save").disabled = false;
    }
  }

  window.ccmuxLLMEditor = { open };
})();
