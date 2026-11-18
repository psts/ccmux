// settingsharness.js: the rules of the Harnesses tab, with no page in it:
// which accounts a harness may use and in what order it would try them,
// what a custom order keeps, and when a row is still the daemon's own
// default. settingsharnesses.js draws the tab from these. Tested in
// settingsharness_test.js.
(function (root) {
  "use strict";

  // The account kinds a harness can be told to use, in the order shown.
  const KINDS = ["anthropic", "openai", "claude", "codex", "meridian"];

  // kindAllowed, mirrored from the daemon (llmproxy.KindAllowed). Empty means
  // any kind except the two a harness has to ask for by name.
  function kindAllowed(kinds, kind) {
    if (!kinds || !kinds.length) return kind !== "codex" && kind !== "meridian";
    return kinds.includes(kind);
  }

  // A codex account speaks OpenAI's Responses API; every other kind speaks
  // Anthropic's Messages API. A harness that checks both gets a warning, not
  // a refusal: an unknown plugin may genuinely need the override, and the
  // proxy holds each pane's failover order to ONE dialect regardless.
  function dialectWarning(kinds) {
    const codex = kinds.includes("codex");
    const other = kinds.some((k) => k !== "codex");
    return codex && other
      ? "codex speaks a different API than the other kinds — a pane will use whichever one answers first and ignore the rest"
      : "";
  }

  // Mirrors llmproxy.subscriptionFirst: a meridian account leads for a
  // harness that declared meridian, because that sidecar spends a Claude
  // subscription rather than a metered key.
  function subscriptionFirst(allowed, kinds) {
    if (!(kinds || []).includes("meridian")) return allowed;
    return allowed.filter((a) => a.kind === "meridian")
      .concat(allowed.filter((a) => a.kind !== "meridian"));
  }

  // The accounts a row may use, in the order it would try them: the ones
  // it named first, then the rest as configured. A named account that no
  // longer exists is dropped here the same way the proxy drops it.
  //
  // subscriptionFirst is mirrored, not skipped. Without it this preview said
  // "1. keyed, 2. sidecar" for opencode while the daemon actually tried the
  // sidecar first — and because turning "custom" on SEEDS the stored order
  // from this list, accepting what the editor showed silently demoted the
  // subscription and moved spend onto a metered key.
  function orderedFor(accounts, kinds, order) {
    const allowed = subscriptionFirst(accounts.filter((a) => kindAllowed(kinds, a.kind)), kinds);
    const named = (order || [])
      .map((n) => allowed.find((a) => a.name === n))
      .filter(Boolean);
    return named.concat(allowed.filter((a) => !named.includes(a)));
  }

  // The order to store once a row shows `ordered`: seeded from what the row
  // resolves to today, so turning "custom" on freezes the current list
  // (which is the whole point of the choice) instead of persisting an empty
  // override that reads as "follow the account order" on the next load.
  //
  // Names already stored are KEPT at their positions even when the current
  // kinds filter them out, so unchecking a kind parks its accounts rather
  // than erasing them: recheck it and the order the user set is still
  // there. The daemon skips a name its kinds do not allow anyway.
  function seeded(stored, ordered) {
    const next = stored.slice();
    for (const a of ordered) if (!next.includes(a.name)) next.push(a.name);
    return next;
  }

  // Swaps two VISIBLE rows (names[i] and its neighbour) inside the stored
  // order. It works on the stored array rather than replacing it with the
  // painted list, because the painted list is kind-filtered: rebuilding from
  // it would drop the accounts an unchecked kind has parked, undoing the
  // whole point of parking them. null when there is nothing to swap.
  function swapped(stored, names, i, delta) {
    const j = i + delta;
    if (j < 0 || j >= names.length) return null;
    const a = stored.indexOf(names[i]), b = stored.indexOf(names[j]);
    if (a < 0 || b < 0) return null; // the row seeds every visible name first
    const next = stored.slice();
    [next[a], next[b]] = [next[b], next[a]];
    return next;
  }

  // What a row holds, as one string to compare. A builtin or detected row
  // that still reads as the daemon sent it is not saved: it stays
  // live-resolved, not frozen.
  function snapshot(h) {
    return JSON.stringify({
      icon: h.icon || "", name: h.name || "", command: h.command || "", autoconfirm: !!h.autoconfirm,
      kinds: (h.accountKinds || []).join(", "), order: (h.accountOrder || []).join(", "),
    });
  }

  const api = { KINDS, kindAllowed, dialectWarning, subscriptionFirst, orderedFor, seeded, swapped, snapshot };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxSettingsHarness = api;
})(typeof window !== "undefined" ? window : this);
