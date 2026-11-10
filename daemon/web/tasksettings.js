// tasksettings.js: the Settings page's Tasks tab, the most queued tasks
// built at once across every window (GET/PUT /v1/settings taskSlotsTotal,
// 0 for no limit). Each window's own number sits in its Tasks header
// (tasks.js); this cap holds on top of those. The bound is the daemon's
// (manager.MaxTaskSlotsTotal). Runs after app.js and uses its $ at call time.
(function (root) {
  "use strict";

  const MAX = 6;

  // capChoices is the picker for a cap of n: No limit (0) to MAX. No number
  // from the daemon (a failed read) means no picker, never a guessed one a
  // save would then write back.
  function capChoices(n) {
    if (typeof n !== "number") return [];
    const out = [];
    for (let v = 0; v <= MAX; v++) {
      out.push({ value: v, label: v === 0 ? "No limit" : `${v} at a time`, selected: v === n });
    }
    return out;
  }

  const api = { capChoices, MAX };
  if (typeof module !== "undefined" && module.exports) { module.exports = api; return; }
  root.ccmuxTaskSettings = api;

  const sel = $("task-cap"), note = $("task-cap-state");
  if (!sel) return;

  function draw(n) {
    const choices = capChoices(n);
    sel.innerHTML = choices.map((c) => `<option value="${c.value}"${c.selected ? " selected" : ""}>${c.label}</option>`).join("");
    sel.disabled = !choices.length;
  }

  async function load() {
    try {
      const r = await fetch("/v1/settings");
      if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || `HTTP ${r.status}`);
      draw((await r.json()).taskSlotsTotal);
      note.textContent = "On top of each window's own number (its Tasks header). A task starts only where both have room.";
    } catch (e) {
      draw(undefined);
      note.textContent = "Couldn't read the setting: " + e.message;
    }
  }

  sel.addEventListener("change", async () => {
    const n = Number(sel.value);
    try {
      const r = await fetch("/v1/settings", {
        method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ taskSlotsTotal: n }),
      });
      if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || `HTTP ${r.status}`);
      draw((await r.json()).taskSlotsTotal);
      note.textContent = n === 0 ? "No limit across windows." : `Up to ${n} at a time across every window.`;
    } catch (e) {
      await load();
      note.textContent = "Couldn't save: " + e.message;
    }
  });

  $("open-settings").addEventListener("click", load);
})(typeof window !== "undefined" ? window : this);
