// settingsharness.js: the rules of the Harnesses tab.
// Run: node --test daemon/web/settingsharness_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const H = require("./settingsharness.js");

const accounts = [
  { name: "keyed", kind: "anthropic" }, { name: "sub", kind: "meridian" },
  { name: "gpt", kind: "openai" }, { name: "cx", kind: "codex" }, { name: "max", kind: "claude" },
];
const names = (list) => list.map((a) => a.name);

test("no kinds checked means every kind but the two a harness must ask for by name", () => {
  assert.equal(H.kindAllowed([], "anthropic"), true);
  assert.equal(H.kindAllowed(undefined, "claude"), true);
  assert.equal(H.kindAllowed([], "codex"), false);
  assert.equal(H.kindAllowed([], "meridian"), false);
  assert.equal(H.kindAllowed(["codex"], "codex"), true);
  assert.equal(H.kindAllowed(["codex"], "anthropic"), false);
});

test("codex beside another kind gets a warning, alone or absent it gets none", () => {
  assert.equal(H.dialectWarning(["codex"]), "");
  assert.equal(H.dialectWarning(["anthropic", "openai"]), "");
  assert.equal(H.dialectWarning([]), "");
  assert.match(H.dialectWarning(["codex", "openai"]), /different API/);
});

test("a harness that declared meridian tries the subscription first", () => {
  assert.deepEqual(names(H.subscriptionFirst(accounts.slice(0, 3), ["meridian", "anthropic"])), ["sub", "keyed", "gpt"]);
  assert.deepEqual(names(H.subscriptionFirst(accounts.slice(0, 3), ["anthropic"])), ["keyed", "sub", "gpt"]);
  assert.deepEqual(names(H.subscriptionFirst(accounts.slice(0, 3), undefined)), ["keyed", "sub", "gpt"]);
});

test("the order a row would try: the accounts it named, then the rest as configured", () => {
  assert.deepEqual(names(H.orderedFor(accounts, [], [])), ["keyed", "gpt", "max"]);
  assert.deepEqual(names(H.orderedFor(accounts, [], ["max", "keyed"])), ["max", "keyed", "gpt"]);
  assert.deepEqual(names(H.orderedFor(accounts, ["meridian", "anthropic", "openai"], undefined)), ["sub", "keyed", "gpt"]);
  assert.deepEqual(names(H.orderedFor(accounts, ["anthropic", "openai"], ["gone", "cx", "gpt"])), ["gpt", "keyed"],
    "a named account that is gone, or of a kind not allowed, is dropped");
  assert.deepEqual(H.orderedFor([], ["codex"], ["cx"]), []);
});

test("turning custom on stores what the row shows, and keeps the names a kind has parked", () => {
  assert.deepEqual(H.seeded([], [{ name: "a" }, { name: "b" }]), ["a", "b"]);
  assert.deepEqual(H.seeded(["parked", "b"], [{ name: "b" }, { name: "a" }]), ["parked", "b", "a"]);
  const stored = ["x"];
  H.seeded(stored, [{ name: "y" }]);
  assert.deepEqual(stored, ["x"], "the stored list handed in is not changed");
});

test("an arrow swaps two shown rows inside the stored order, parked names staying put", () => {
  assert.deepEqual(H.swapped(["a", "parked", "b"], ["a", "b"], 0, 1), ["b", "parked", "a"]);
  assert.deepEqual(H.swapped(["a", "parked", "b"], ["a", "b"], 1, -1), ["b", "parked", "a"]);
  assert.equal(H.swapped(["a", "b"], ["a", "b"], 0, -1), null);
  assert.equal(H.swapped(["a", "b"], ["a", "b"], 1, 1), null);
  assert.equal(H.swapped(["a"], ["a", "b"], 0, 1), null, "a shown name that is not stored yet is not swapped");
});

test("a row reads as untouched only while it holds what the daemon sent", () => {
  const sent = { name: "claude", icon: "✳", command: "claude", autoconfirm: true, accountKinds: ["anthropic", "claude"], source: "builtin" };
  const read = { icon: "✳", name: "claude", command: "claude", autoconfirm: true, accountKinds: ["anthropic", "claude"] };
  assert.equal(H.snapshot(read), H.snapshot(sent));
  assert.notEqual(H.snapshot({ ...read, command: "claude --flag" }), H.snapshot(sent));
  assert.notEqual(H.snapshot({ ...read, accountOrder: ["keyed"] }), H.snapshot(sent));
  assert.notEqual(H.snapshot({ ...read, autoconfirm: false }), H.snapshot(sent));
  assert.equal(H.snapshot({}), H.snapshot({ icon: "", name: "", command: "", autoconfirm: false }));
});
