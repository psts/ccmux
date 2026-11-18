// threadsideprefs.js: each person's own thread-page sidebar, as rules.
// The daemon keeps, per login, which groups (windows) are folded into their
// header line and where each sits (GET /v1/me/sidebar); this file turns
// that into the order the sidebar draws, the order a drag or "Move up"
// sends back, and the alert a folded group's header shows. A group is the
// threadmodel tree's: {id, name, repos}; "Other" (repos in no window) has
// no id, is never folded or moved, and is always last.
//
// Plain functions over plain data, so threadsideprefs_test.js runs them
// without a browser. Every function returns new values; none changes its
// arguments.
(function (root) {
  "use strict";

  // posOf is a group's place in prefs, null when the person never placed it.
  function posOf(prefs, id) {
    const p = (prefs || []).find((x) => x.windowId === id);
    return p && typeof p.position === "number" ? p.position : null;
  }

  // arrange is the groups in the person's order: placed groups by their
  // place, then the rest in the order they came (the page's default), then
  // "Other". alone (the ?g= of a page opened for one group) keeps only that
  // group; an alone that names no group keeps them all, so a stale link
  // still shows something.
  function arrange(groups, prefs, alone) {
    const list = groups || [];
    if (alone && list.some((g) => g.id === alone)) return list.filter((g) => g.id === alone);
    const keyed = list.map((g, i) => ({ g, i, pos: g.id ? posOf(prefs, g.id) : null }));
    const rank = (k) => (!k.g.id ? 2 : k.pos === null ? 1 : 0);
    keyed.sort((a, b) => rank(a) - rank(b) || (rank(a) === 0 ? a.pos - b.pos : 0) || a.i - b.i);
    return keyed.map((k) => k.g);
  }

  // folded says whether a group shows as its header alone. Never "Other",
  // and never on a page opened for that one group.
  function folded(prefs, id, alone) {
    if (!id || alone) return false;
    const p = (prefs || []).find((x) => x.windowId === id);
    return !!(p && p.collapsed);
  }

  // withFolded is prefs with one group folded or not, its place kept.
  function withFolded(prefs, id, on) {
    const list = (prefs || []).map((p) => ({ ...p }));
    const p = list.find((x) => x.windowId === id);
    if (p) p.collapsed = !!on;
    else list.push({ windowId: id, collapsed: !!on, position: null });
    return list;
  }

  // withOrder is prefs with ids placed 0..n and every other group unplaced,
  // as the daemon stores PUT /v1/me/sidebar/order.
  function withOrder(prefs, ids) {
    const list = (prefs || []).map((p) => ({ ...p, position: null }));
    ids.forEach((id, i) => {
      const p = list.find((x) => x.windowId === id);
      if (p) p.position = i;
      else list.push({ windowId: id, collapsed: false, position: i });
    });
    return list;
  }

  // ids are the movable groups' ids in the order on screen.
  const ids = (arranged) => arranged.filter((g) => g.id).map((g) => g.id);

  // moveBy is the order with id moved delta places (-1 up, +1 down), null
  // when it cannot move that way.
  function moveBy(order, id, delta) {
    const from = order.indexOf(id);
    const to = from + delta;
    if (from < 0 || to < 0 || to >= order.length) return null;
    const out = order.filter((x) => x !== id);
    out.splice(to, 0, id);
    return out;
  }

  // moveTo is the order with id dropped just before before ("" for the
  // end), null when that leaves the order as it was.
  function moveTo(order, id, before) {
    if (!order.includes(id) || id === before) return null;
    const out = order.filter((x) => x !== id);
    const at = before ? out.indexOf(before) : out.length;
    if (at < 0) return null;
    out.splice(at, 0, id);
    return out.join("\n") === order.join("\n") ? null : out;
  }

  // dropBefore is the group a drop lands in front of, given the group
  // under the pointer and whether the pointer is in its lower half: the
  // next one down then ("" past the last).
  function dropBefore(order, overId, lower) {
    const i = order.indexOf(overId);
    if (i < 0) return null;
    return lower ? order[i + 1] || "" : overId;
  }

  // alert is what a folded group's header says about what is inside it:
  // "waiting" (orange) when anything needs someone, else "done" (green)
  // when anything finished unseen, with how many; the same states that
  // make a row pulse (threadview attClass). taskAlerts are the group's
  // queued tasks that need a person, and count as waiting.
  function alert(g, taskAlerts) {
    let waiting = taskAlerts || 0, done = 0;
    for (const r of (g && g.repos) || []) {
      for (const s of [r.state, ...(r.threads || []).map((t) => t.state)]) {
        if (s === "waiting") waiting++;
        else if (s === "done") done++;
      }
    }
    if (waiting) return { cls: "waiting", count: waiting, text: `${waiting} need${waiting === 1 ? "s" : ""} you` };
    if (done) return { cls: "done", count: done, text: `${done} done, not seen` };
    return { cls: "", count: 0, text: "" };
  }

  const api = { arrange, folded, withFolded, withOrder, ids, moveBy, moveTo, dropBefore, alert };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadSidePrefs = api;
})(typeof window !== "undefined" ? window : this);
