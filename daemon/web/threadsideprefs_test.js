// node --test threadsideprefs_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const P = require("./threadsideprefs.js");

const g = (id, name, repos = []) => ({ id, name, repos });
const groups = [g("a", "ALPHA"), g("b", "BETA"), g("d", "DASHA"), g("", "Other")];
const names = (list) => list.map((x) => x.name).join(",");

test("placed groups come first by place, the rest keep the default order, Other is last", () => {
  assert.equal(names(P.arrange(groups, [])), "ALPHA,BETA,DASHA,Other");
  const prefs = [{ windowId: "d", collapsed: true, position: 0 }, { windowId: "a", collapsed: false, position: null }];
  assert.equal(names(P.arrange(groups, prefs)), "DASHA,ALPHA,BETA,Other");
  const all = [{ windowId: "b", position: 5 }, { windowId: "d", position: 2 }, { windowId: "a", position: 9 }];
  assert.equal(names(P.arrange(groups, all)), "DASHA,BETA,ALPHA,Other", "places are compared, not counted");
  assert.equal(names(P.arrange([g("", "Other"), g("a", "ALPHA")], [])), "ALPHA,Other");
  assert.equal(names(groups), "ALPHA,BETA,DASHA,Other", "the input is not changed");
});

test("a page opened for one group shows that group alone, and a stale one shows them all", () => {
  assert.equal(names(P.arrange(groups, [], "d")), "DASHA");
  assert.equal(names(P.arrange(groups, [], "gone")), "ALPHA,BETA,DASHA,Other");
});

test("folding is per group, never Other, and off on a page opened for one group", () => {
  const prefs = P.withFolded([], "d", true);
  assert.equal(P.folded(prefs, "d"), true);
  assert.equal(P.folded(prefs, "a"), false);
  assert.equal(P.folded(prefs, ""), false);
  assert.equal(P.folded(prefs, "d", "d"), false);
  const placed = P.withFolded([{ windowId: "d", collapsed: false, position: 3 }], "d", true);
  assert.deepEqual(placed, [{ windowId: "d", collapsed: true, position: 3 }], "folding keeps the place");
  assert.equal(P.folded(P.withFolded(prefs, "d", false), "d"), false);
});

test("a new order places the listed groups and unplaces the rest, folds kept", () => {
  const before = [{ windowId: "a", collapsed: true, position: 0 }, { windowId: "b", collapsed: false, position: 1 }];
  const after = P.withOrder(before, ["d", "a"]);
  assert.deepEqual(after, [
    { windowId: "a", collapsed: true, position: 1 }, { windowId: "b", collapsed: false, position: null },
    { windowId: "d", collapsed: false, position: 0 },
  ]);
  assert.equal(before[0].position, 0, "the input is not changed");
});

test("Move up and Move down move one place, and not past either end", () => {
  const order = P.ids(P.arrange(groups, []));
  assert.deepEqual(order, ["a", "b", "d"]);
  assert.deepEqual(P.moveBy(order, "d", -1), ["a", "d", "b"]);
  assert.deepEqual(P.moveBy(order, "a", 1), ["b", "a", "d"]);
  assert.equal(P.moveBy(order, "a", -1), null);
  assert.equal(P.moveBy(order, "d", 1), null);
  assert.equal(P.moveBy(order, "x", 1), null);
});

test("a drop lands before the group under the pointer, or after it from its lower half", () => {
  const order = ["a", "b", "d"];
  assert.equal(P.dropBefore(order, "b", false), "b");
  assert.equal(P.dropBefore(order, "b", true), "d");
  assert.equal(P.dropBefore(order, "d", true), "");
  assert.equal(P.dropBefore(order, "", true), null, "Other is no drop target");
  assert.deepEqual(P.moveTo(order, "d", "a"), ["d", "a", "b"]);
  assert.deepEqual(P.moveTo(order, "a", ""), ["b", "d", "a"]);
  assert.equal(P.moveTo(order, "a", "b"), null, "dropped where it already is");
  assert.equal(P.moveTo(order, "a", "a"), null);
  assert.equal(P.moveTo(order, "x", "a"), null);
});

test("a folded group's alert: waiting beats done, threads and needy tasks count", () => {
  const repo = (state, ...threads) => ({ state, threads: threads.map((s) => ({ state: s })) });
  assert.deepEqual(P.alert(g("d", "DASHA", [repo("running"), repo("idle", "asleep")])), { cls: "", count: 0, text: "" });
  assert.deepEqual(P.alert(g("d", "DASHA", [repo("done", "done")])), { cls: "done", count: 2, text: "2 done, not seen" });
  assert.deepEqual(P.alert(g("d", "DASHA", [repo("done"), repo("running", "waiting")])), { cls: "waiting", count: 1, text: "1 needs you" });
  assert.deepEqual(P.alert(g("d", "DASHA", [repo("waiting")]), 2), { cls: "waiting", count: 3, text: "3 need you" });
  assert.deepEqual(P.alert(g("d", "DASHA", []), 1), { cls: "waiting", count: 1, text: "1 needs you" });
});
