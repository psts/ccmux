// Rules of route.js: which window a browser window shows, and what a relaunch
// brings back. Run: node --test daemon/web/route_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const R = require("./route.js");

function memStorage() {
  const m = new Map();
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)) };
}

test("the address round-trips window and session", () => {
  assert.deepStrictEqual(R.parse(R.href("win-1", "ws-2").slice(1)), { w: "win-1", ws: "ws-2" });
  assert.strictEqual(R.href("", ""), "/");
  assert.deepStrictEqual(R.parse("?ws=ws-9"), { w: "", ws: "ws-9" }); // a notification link from before
});

test("a session's window comes from the shared membership", () => {
  const windows = [{ id: "a", workspaceIds: ["s1"] }, { id: "b", workspaceIds: ["s2", "s3"] }];
  assert.strictEqual(R.windowOf(windows, "s3"), "b");
  assert.strictEqual(R.windowOf(windows, "loose"), "");
});

test("each window remembers its own last session", () => {
  const s = memStorage();
  R.remember(s, "a", "s1");
  R.remember(s, "b", "s2");
  assert.strictEqual(R.recall(s, "a"), "s1");
  assert.strictEqual(R.recall(s, "b"), "s2");
  assert.strictEqual(R.recall(s, "c"), "");
});

test("a quit brings back every window that was open, once", () => {
  const s = memStorage();
  R.beat(s, "p1", "mixed", 1000);
  R.beat(s, "p2", "chartlabs", 1000);
  R.ended(s, "p1", 5000);
  R.ended(s, "p2", 5001);
  assert.deepStrictEqual(R.takeRestoreSet(s, "new", new Set()), ["mixed", "chartlabs"]);
  assert.deepStrictEqual(R.takeRestoreSet(s, "new", new Set()), []);
});

test("a window closed by hand long before the quit stays closed", () => {
  const s = memStorage();
  R.beat(s, "p1", "mixed", 0);
  R.beat(s, "p2", "chartlabs", 0);
  R.ended(s, "p1", 1000);
  R.ended(s, "p2", 1000 + R.BURST_MS + 1);
  assert.deepStrictEqual(R.takeRestoreSet(s, "new", new Set()), ["chartlabs"]);
});

test("Cmd+N while other windows are open restores nothing and forgets nothing", () => {
  const s = memStorage();
  R.beat(s, "p1", "mixed", 0);
  assert.deepStrictEqual(R.takeRestoreSet(s, "new", new Set(["p1"])), []);
  R.ended(s, "p1", 10);
  assert.deepStrictEqual(R.takeRestoreSet(s, "new", new Set()), ["mixed"]);
});

test("a reload is not a goodbye", () => {
  const s = memStorage();
  R.beat(s, "p1", "mixed", 0);
  R.ended(s, "p1", 10);
  R.beat(s, "p1", "mixed", 11); // the same browser window, reloaded
  assert.deepStrictEqual(R.takeRestoreSet(s, "p1", new Set()), []);
});

test("a window that crashed counts as gone at its last beat; the picker is not restored", () => {
  const s = memStorage();
  R.beat(s, "p1", "mixed", 100);
  R.beat(s, "p2", "", 100);
  assert.deepStrictEqual(R.takeRestoreSet(s, "new", new Set()), ["mixed"]);
});

test("unreadable storage means nothing remembered, never a throw", () => {
  const broken = { getItem: () => { throw new Error("blocked"); }, setItem: () => { throw new Error("blocked"); } };
  assert.strictEqual(R.recall(broken, "a"), "");
  R.beat(broken, "p1", "a", 1);
  assert.deepStrictEqual(R.takeRestoreSet(broken, "p1", new Set()), []);
});

test("a browser window keeps its window unless the session belongs to another", () => {
  const windows = [{ id: "a", workspaceIds: ["s1"] }, { id: "b", workspaceIds: ["s2"] }];
  assert.strictEqual(R.bootTarget(windows, "a", "s2"), "b"); // s2 moved to b after the link was made
  assert.strictEqual(R.bootTarget(windows, "gone", "s1"), "a"); // window deleted
  assert.strictEqual(R.bootTarget(windows, "a", ""), "a");
  assert.strictEqual(R.bootTarget(windows, "gone", ""), "");
  // A session in no window, live (AVAILABLE) or deleted, leaves the address's
  // window to stand; pagewin.js decides whether it can be shown there.
  assert.strictEqual(R.bootTarget(windows, "a", "loose"), "a");
  assert.strictEqual(R.bootTarget(windows, "a", "deleted"), "a");
  assert.strictEqual(R.bootTarget(windows, "", "loose"), "");
  assert.strictEqual(R.bootTarget(windows, "gone", "deleted"), "");
});

test("a missed tap is taken by whoever reads it, unless it is stale", () => {
  const now = 1000000;
  const tap = (t) => `/?w=a&ws=s1&t=${t}`;
  assert.strictEqual(R.takesNav(tap(now), now), "take");
  assert.strictEqual(R.takesNav(tap(now - R.NAV_TTL_MS), now), "take"); // exactly at the limit
  assert.strictEqual(R.takesNav(tap(now - R.NAV_TTL_MS - 1), now), "drop");
  assert.strictEqual(R.takesNav("/?ws=s1", now), "take"); // a stash from before windows
});

test("a reload knows its page; a new window does not", () => {
  const s = memStorage();
  assert.strictEqual(R.known(s, "p1"), false);
  R.beat(s, "p1", "", 1);
  assert.strictEqual(R.known(s, "p1"), true);
  assert.strictEqual(R.known(s, "p2"), false);
});
