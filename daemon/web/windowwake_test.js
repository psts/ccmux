// Which sessions a window's close records and its open wakes (windowwake.js).
// Run: node --test daemon/web/windowwake_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const W = require("./windowwake.js");

const sessions = { a: { status: "live" }, b: { status: "cold" }, c: { status: "cold" } };
const byId = (id) => sessions[id];
const members = ["a", "b", "c"];

test("a close records the members that are awake", () => {
  assert.deepStrictEqual(W.awakeAt(members, byId), ["a"]);
  assert.deepStrictEqual(W.awakeAt(["a", "gone"], byId), ["a"]);
  assert.deepStrictEqual(W.awakeAt([], byId), []);
});

test("an open wakes only the marked members that are asleep", () => {
  assert.deepStrictEqual(W.toWake({ wake: ["b"] }, members, byId), ["b"]);
  assert.deepStrictEqual(W.toWake({ wake: [] }, members, byId), []);
});

test("a marked session that is already awake, or no longer a member, is not woken", () => {
  assert.deepStrictEqual(W.toWake({ wake: ["a", "x", "c"] }, members, byId), ["c"]);
});

test("an answer with no list wakes nothing", () => {
  assert.deepStrictEqual(W.toWake({}, members, byId), []);
  assert.deepStrictEqual(W.toWake(null, members, byId), []);
});
