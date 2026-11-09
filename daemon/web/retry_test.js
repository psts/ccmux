// How long a dropped connection waits before it dials again (retry.js).
// Run: node --test daemon/web/retry_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const R = require("./retry.js");

test("the first redial waits 2s, as every socket did before", () => {
  assert.strictEqual(R.backoff().next(), 2000);
});

test("each failure in a row doubles the wait, up to 30s and no further", () => {
  const b = R.backoff();
  const waits = Array.from({ length: 7 }, () => b.next());
  assert.deepStrictEqual(waits, [2000, 4000, 8000, 16000, 30000, 30000, 30000]);
});

test("a connection that opens starts the count over", () => {
  const b = R.backoff();
  b.next(); b.next(); b.next();
  b.ok();
  assert.strictEqual(b.next(), 2000);
});

test("each connection counts its own failures", () => {
  const a = R.backoff();
  const b = R.backoff();
  a.next(); a.next(); a.next();
  assert.strictEqual(b.next(), 2000);
  assert.strictEqual(a.next(), 16000);
});
