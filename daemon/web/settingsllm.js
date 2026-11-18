// settingsllm.js: what the Accounts tab's words and list changes mean, with
// no page in it: the health line under an account, the key box's hint, the
// alias field's text, and the three changes a click makes to the account
// list. settingsaccounts.js draws the tab and settingsllmedit.js the
// one-account editor; both read these. Tested in settingsllm_test.js.
(function (root) {
  "use strict";

  const KINDS = ["anthropic", "openai", "claude", "codex", "meridian"];

  const STATES = {
    ok: "● active", limited: "◐ limited", unauthorized: "✕ credential rejected",
    untried: "○ no traffic yet", unreachable: "⚠ not answering", failing: "⚠ answering errors",
  };

  const when = (iso) => (iso ? new Date(iso).toLocaleString([], { weekday: "short", hour: "2-digit", minute: "2-digit" }) : "");

  // Usage percentages where the upstream sends them (Anthropic
  // subscriptions: session = 5h window, week = 7 days).
  function usageOf(st) {
    const usage = [];
    if (st.sessionPct >= 0) usage.push(`session ${st.sessionPct}%${st.sessionReset ? " (resets " + when(st.sessionReset) + ")" : ""}`);
    if (st.weeklyPct >= 0) usage.push(`week ${st.weeklyPct}%`);
    return usage;
  }

  // One line of live health under an account: reachable, limited (and until
  // when), token rejected, upstream not answering, upstream answering errors
  // — plus usage where there is any.
  function statusLine(st) {
    if (!st) return "";
    let s = STATES[st.state] || st.state;
    if (st.state === "limited" && st.limitedUntil) s += " until " + when(st.limitedUntil);
    // The daemon sends lastError only WHILE the account is unreachable or
    // failing, so there is no stale-text case to guard here.
    if (st.lastError) s += " (" + st.lastError + ")";
    const usage = usageOf(st);
    if (usage.length) s += " · " + usage.join(" · ");
    return s;
  }

  // The sidecar footer of a meridian account: the daemon runs one Meridian
  // process per such account (its key is the Claude setup token that process
  // authenticates with), so "is it running" is the status that matters.
  function sidecarLine(a, sc) {
    if (a.kind !== "meridian") return "";
    if (!sc) return "⚙ sidecar not supervised by this daemon";
    if (sc.running) return `⚙ sidecar running (pid ${sc.pid}${sc.restarts ? `, ${sc.restarts} restarts` : ""})`;
    return "⚙ sidecar stopped" + (sc.lastError ? ": " + sc.lastError : "");
  }

  // Both lines of one account, as the row and the editor show them.
  const healthLine = (a, st, sc) => [statusLine(st), sidecarLine(a, sc)].filter(Boolean).join(" · ");

  function keyHintFor(a) {
    if (a.apiKeySet) return "set — empty keeps it";
    if (a.kind === "claude") return "paste `claude setup-token` output";
    if (a.kind === "meridian") return "paste `claude setup-token` output (starts the sidecar)";
    return "empty = your own login";
  }

  // Only a claude account holding its own setup-token reports quota, and the
  // daemon's own row is what says so (llmproxy/probe.go checkable): an
  // account it has no row for is one it would not know the name of. Same
  // rule as the Mac's checkable().
  const checkable = (st) => !!st && st.kind === "claude" && st.credentialSet;

  function parseAliases(text) {
    return text.split(",").map((part) => {
      const [from, ...rest] = part.split("=");
      return { from: (from || "").trim(), to: rest.join("=").trim() };
    }).filter((x) => x.from && x.to);
  }

  const aliasText = (list) => (list || []).map((x) => `${x.from}=${x.to}`).join(", ");

  // Picking a model rewrites the alias field: claude-* rules replaced,
  // custom rules kept.
  function aliasesWithModel(text, model) {
    const kept = parseAliases(text).filter((x) => !x.from.startsWith("claude-"));
    return aliasText(kept.concat([{ from: "claude-*", to: model }]));
  }

  // The model every claude-* request is mapped to, "" when there is none.
  const mappedModel = (a) => ((a.modelAliases || []).find((x) => x.from === "claude-*") || {}).to || "";

  // The three list changes below take the daemon's CURRENT list (the save
  // queue re-reads it) and address an account by NAME, never by the index a
  // row was drawn with: a save is a round trip, and a second click during
  // one would otherwise build its change from the list as it was. Each
  // returns the list to send, null for "nothing to send", or throws for a
  // change that can no longer be made.

  // The stored order IS the failover order, so moving a row is a real setting.
  function moved(list, name, delta) {
    const at = list.findIndex((x) => x.name === name);
    if (at < 0) {
      // The row outlived the account (a queued delete, another lens).
      throw new Error(`"${name}" is no longer in the list.`);
    }
    const to = at + delta;
    if (to < 0 || to >= list.length) return null; // already at the end
    const next = list.slice();
    [next[at], next[to]] = [next[to], next[at]];
    return next;
  }

  const removed = (list, name) => list.filter((x) => x.name !== name);

  // An edit lands on the account the editor was opened on (wasNamed), a new
  // account ("") at the end.
  function upserted(list, wasNamed, next) {
    const at = wasNamed ? list.findIndex((x) => x.name === wasNamed) : -1;
    if (wasNamed && at < 0) {
      // It was deleted while the sheet was open (a queued delete, another
      // tab, the Mac lens). Pushing it back would re-create it with no
      // key — the daemon inherits a stored key BY NAME and that name is
      // gone — so it would return as a pass-through forwarding every
      // pane's own login. Fail loudly; the sheet stays open.
      throw new Error(`"${wasNamed}" was deleted while you had it open — its stored key is gone. Re-add it to set a new one.`);
    }
    const candidate = list.slice();
    if (at >= 0) candidate[at] = next;
    else candidate.push(next);
    return candidate;
  }

  const api = { KINDS, statusLine, sidecarLine, healthLine, keyHintFor, checkable, parseAliases, aliasText, aliasesWithModel, mappedModel, moved, removed, upserted };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxSettingsLLM = api;
})(typeof window !== "undefined" ? window : this);
