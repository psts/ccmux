// threadcaps.js: what the thread page says about skills and plugins. The
// daemon serves the catalog with its git sources (/v1/capabilities), every
// group's and repo's choices (/v1/capabilities/choices), and per repo or
// group what it has on (/v1/workspaces/{id}/capabilities,
// /v1/windows/{id}/capabilities); this turns them into the rows of the
// Settings tab and of the panel a right-click on a repo or group opens.
//
// Plain functions over plain data, so threadcaps_test.js runs them without
// a browser. threadcapsview.js draws what these return; threadcapsctl.js
// fetches and wires.
(function (root) {
  "use strict";

  const kinds = { plugin: "plugin", skill: "skill", mcp: "MCP", connector: "connector" };
  const kindLabel = (kind) => kinds[kind] || kind || "?";

  // name is what an entry is called on screen: a connector by its own
  // name without the "claude.ai " Claude Code puts in front, else its id.
  const name = (c) => (c.connector ? c.connector.replace(/^claude\.ai /, "") : c.id);

  // tokens is the context an entry adds, "~1.4k tokens"; "" when the
  // daemon has no estimate.
  const tokens = (n) => (n >= 50 ? `~${(n / 1000).toFixed(1)}k tokens` : n > 0 ? "<0.1k tokens" : "");

  // brief is an entry's description cut to one line: its first sentence,
  // and no more than about a line's worth of it.
  function brief(text) {
    const first = String(text || "").split(/(?<=[.!?])\s|\n/)[0].trim().replace(/[.!?]$/, "");
    return first.length > 90 ? first.slice(0, 89).replace(/\s+\S*$/, "") + "…" : first;
  }

  // shortURL is a source's address without the scheme and host when it
  // is on GitHub ("anthropics/claude-plugins-official"), else as it is.
  function shortURL(url) {
    const m = /^https:\/\/github\.com\/(.+?)(\.git)?\/?$/.exec(url || "");
    return m ? m[1] : url || "";
  }

  // sourceId is the id a new source gets from its address: its last
  // part, in the letters the catalog takes (capabilities.go capabilityID).
  // "" when nothing usable is left.
  function sourceId(url) {
    const last = String(url || "").trim().replace(/\/+$/, "").replace(/\.git$/, "").split(/[/:]/).pop() || "";
    // A leading dot goes, so the id is never the pins folder (".pins").
    return last.replace(/[^A-Za-z0-9_.-]+/g, "-").replace(/^[^A-Za-z0-9]+/, "").slice(0, 64);
  }

  // checked is when a source was last fetched, in days.
  function checked(updated, now) {
    const at = Date.parse(updated || "");
    if (!at) return { text: "never checked", days: Infinity };
    const days = Math.max(0, Math.floor((now - at) / 86400000));
    return { text: days === 0 ? "checked today" : days === 1 ? "checked yesterday" : `checked ${days} days ago`, days };
  }

  // sourceRow is one source in the Settings tab. Nothing tells the page a
  // source has moved on upstream before it is fetched, so "N updates" is
  // what is known: the pinned entries from it whose source has a newer
  // version. "Up to date" only for a source checked this week.
  function sourceRow(src, caps, now) {
    const behind = (caps || []).filter((c) => c.source === src.id && c.updateAvailable).length;
    const when = checked(src.updated, now);
    const where = ["git", shortURL(src.url), src.commit ? src.commit.slice(0, 7) : "", when.text].filter(Boolean).join(" · ");
    let tag = null;
    if (behind) tag = { text: `${behind} update${behind === 1 ? "" : "s"}`, tone: "warn" };
    else if (when.days <= 7) tag = { text: "Up to date", tone: "ok" };
    return { id: src.id, name: src.id, where, tag, behind };
  }

  // placesOn says where an entry is on: in which groups, and in how many
  // repos on their own.
  function placesOn(id, choices, windowNames) {
    const groups = Object.entries((choices && choices.windows) || {}).filter(([, on]) => (on || []).includes(id)).map(([w]) => windowNames[w] || w);
    const repos = Object.values((choices && choices.workspaces) || {}).filter((c) => c && c[id] === "on").length;
    const parts = [];
    if (groups.length === 1) parts.push("on in " + groups[0]);
    else if (groups.length) parts.push(`on in ${groups.length} groups`);
    if (repos) parts.push(`on in ${repos} repo${repos === 1 ? "" : "s"}`);
    return parts.length ? parts.join(", ") : "on nowhere";
  }

  // pinnable mirrors the daemon's fromCheckout (capsync.go): only an
  // entry that came from a source's checkout can be pinned.
  const pinnable = (c) => !!c.commit && !!c.source;

  function versionNote(c) {
    if (c.pinned) return "pinned at " + (c.version || (c.commit || "").slice(0, 7));
    if (!pinnable(c)) return c.version || "";
    return (c.version ? c.version + " · " : "") + "follows newest";
  }

  // entryRow is one entry in the Settings tab's list. Its source is apart:
  // the list is grouped by it (entryGroups), so the line under it leaves it
  // out.
  function entryRow(c, choices, windowNames) {
    const meta = [brief(c.description), tokens(c.cost), versionNote(c), placesOn(c.id, choices, windowNames)].filter(Boolean).join(" · ");
    return {
      id: c.id, name: name(c), kind: kindLabel(c.kind), source: c.source || "", meta,
      update: c.updateAvailable ? `update ${c.version || "?"} → ${c.updateAvailable}` : "",
      pin: pinnable(c) ? (c.pinned ? "Unpin" : "Pin") : "",
    };
  }

  // ORIGINS are the sources that are not git sources, in the order they
  // come after those (agent.SourceClaudeAI, agent.SourceThisMachine).
  const ORIGINS = ["claude.ai", "this machine"];

  // entryGroups puts the Settings tab's rows under their sources: the git
  // sources in the order the tab lists them, then claude.ai and this
  // machine, then any other (a marketplace with no git source here) by
  // name, and last the rows with no source, as "Other". A source with no
  // rows gets no section; rows keep their order inside one.
  function entryGroups(rows, sources) {
    const by = new Map();
    for (const r of rows) {
      if (!by.has(r.source)) by.set(r.source, []);
      by.get(r.source).push(r);
    }
    const known = sources.map((s) => s.id).concat(ORIGINS);
    const rest = [...by.keys()].filter((id) => id && !known.includes(id)).sort();
    return known.concat(rest, [""]).filter((id, i, all) => by.has(id) && all.indexOf(id) === i)
      .map((id) => ({ id, name: id || "Other", rows: by.get(id) }));
  }

  // repoNote is what a repo's row says of its group, beside its choice.
  function repoNote(choice, groupOn, group) {
    if (!group) return choice === "on" ? "On here" : choice === "off" ? "Off here" : "Off, this repo is in no group";
    if (choice === "on") return groupOn ? `On in ${group} and here` : `Off in ${group}, on here`;
    if (choice === "off") return groupOn ? `On in ${group}, off here` : `Off in ${group} and here`;
    return groupOn ? "On from " + group : "Off from " + group;
  }

  // gone are the ids a list names that the catalog no longer holds, as
  // rows that say so: a session skips them, and Off clears them.
  function gone(caps, ids) {
    const known = new Set(caps.map((c) => c.id));
    return [...new Set(ids)].filter((id) => !known.has(id)).sort().map((id) => ({ id, name: id, kind: "", gone: true }));
  }

  // repoRows is the panel of one repo: every catalog entry with the
  // repo's own choice (group, on or off) and what that comes to.
  // data is GET /v1/workspaces/{id}/capabilities.
  function repoRows(caps, data, group) {
    const groupOn = new Set(data.group || []);
    const choices = data.choices || {};
    const list = (caps || []).map((c) => ({ id: c.id, name: name(c), kind: kindLabel(c.kind) }));
    const rows = list.concat(gone(caps || [], Object.keys(choices)));
    return rows.map((r) => {
      const state = choices[r.id] || "group";
      const note = r.gone ? "Gone from the list, sessions skip it" : repoNote(state, groupOn.has(r.id), group);
      return { ...r, state, note };
    });
  }

  // groupRows is the panel of one group: every entry, on or off. data is
  // GET /v1/windows/{id}/capabilities.
  function groupRows(caps, data) {
    const on = new Set(data.on || []);
    const rows = (caps || []).map((c) => ({ id: c.id, name: name(c), kind: kindLabel(c.kind) })).concat(gone(caps || [], [...on]));
    return rows.map((r) => ({ ...r, state: on.has(r.id) ? "on" : "off", note: r.gone ? "Gone from the list, sessions skip it" : "" }));
  }

  // cost is what the ids on add to every session, in tokens.
  function cost(caps, ids) {
    const on = new Set(ids || []);
    return (caps || []).reduce((sum, c) => sum + (on.has(c.id) ? c.cost || 0 : 0), 0);
  }

  // stale are the sessions in these repos running on a list that has
  // moved on (the daemon's loadoutStale, set only for one that should be
  // running).
  function stale(sessions, workspaceIds) {
    const ids = new Set(workspaceIds || []);
    return (sessions || []).filter((s) => s.loadoutStale && ids.has(s.workspaceId));
  }

  // footLine is the line under a panel's list: what its sessions load,
  // and when a change reaches them.
  function footLine(total, who) {
    const load = total > 0 ? `${who} sessions load about ${(total / 1000).toFixed(1)}k tokens of these.` : `${who} sessions load none of these.`;
    return load + " Changes apply when a session next starts.";
  }

  const staleLine = (n) => (n ? `${n} running session${n === 1 ? " uses" : "s use"} the old list.` : "");

  // updateNote is what an Update answered, in one line.
  function updateNote(res) {
    const n = (k) => (res[k] || []).length;
    const parts = [];
    if (n("changed")) parts.push(`${n("changed")} changed`);
    if (n("added")) parts.push(`${n("added")} new`);
    if (n("gone")) parts.push(`${n("gone")} gone`);
    if (n("behind")) parts.push(`${n("behind")} pinned behind`);
    const id = (res.source && res.source.id) || "source";
    return `${id}: ${parts.length ? parts.join(", ") : "nothing changed"}.`;
  }

  const api = { kindLabel, name, tokens, brief, shortURL, sourceId, checked, sourceRow, placesOn, pinnable, entryRow, entryGroups, repoNote, repoRows, groupRows, cost, stale, footLine, staleLine, updateNote };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadCaps = api;
})(typeof window !== "undefined" ? window : this);
