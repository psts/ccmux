// When an open page moves onto a newer web lens (update.js).
// Run: node --test daemon/web/update_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const U = require("./update.js");

const idle = { returning: true, busy: false, hidden: false };

test("the same build, or a build nobody knows, changes nothing", () => {
  assert.strictEqual(U.decide("aaa", "aaa", idle), "none");
  assert.strictEqual(U.decide(undefined, "bbb", idle), "none"); // a page from before builds
  assert.strictEqual(U.decide("aaa", "", idle), "none"); // the daemon did not answer
});

test("a page coming back into view with nothing going on reloads", () => {
  assert.strictEqual(U.decide("aaa", "bbb", idle), "reload");
});

test("a page in use gets the bar instead", () => {
  assert.strictEqual(U.decide("aaa", "bbb", { ...idle, busy: true }), "bar"); // unsent text, open dialog
  assert.strictEqual(U.decide("aaa", "bbb", { ...idle, returning: false }), "bar"); // on screen all along
});

test("a hidden page waits until it is shown", () => {
  assert.strictEqual(U.decide("aaa", "bbb", { ...idle, hidden: true }), "none");
});
