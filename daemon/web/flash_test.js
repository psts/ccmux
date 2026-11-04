// How long a row's attention flash lasts (flash.js).
// Run: node --test daemon/web/flash_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const F = require("./flash.js");

const T = 1_800_000_000_000; // a finish time, unix ms

test("a repo session's green flashes until someone looks, however long ago it finished", () => {
  assert.strictEqual(F.doneShows(false, T, T + 10 * 60000), true);
});

test("an agent's green flashes for its first minute, then goes off", () => {
  assert.strictEqual(F.doneShows(true, T, T), true);
  assert.strictEqual(F.doneShows(true, T, T + F.AGENT_DONE_MS - 1), true);
  assert.strictEqual(F.doneShows(true, T, T + F.AGENT_DONE_MS), false);
  assert.strictEqual(F.doneShows(true, T, T + 10 * 60000), false);
});

test("an agent claim with no start time (an older daemon) flashes as before", () => {
  assert.strictEqual(F.doneShows(true, 0, T), true);
});

test("a browser clock behind the daemon's still flashes a fresh finish", () => {
  assert.strictEqual(F.doneShows(true, T, T - 5000), true);
});
