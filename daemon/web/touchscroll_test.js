// Rules of touchscroll.js: which way a finger drag turns the wheel, and how
// fast a flick keeps gliding. Run: node --test daemon/web/touchscroll_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const T = require("./touchscroll.js");

test("dragging the content down scrolls back, as a wheel turned up does", () => {
  assert.strictEqual(T.wheelFor(40, 10, 20).deltaY, -40); // finger down: older lines
  assert.strictEqual(T.wheelFor(-40, 10, 20).deltaY, 40); // finger up: newer lines
  const w = T.wheelFor(12, 100, 200);
  assert.deepStrictEqual([w.deltaMode, w.clientX, w.clientY, w.bubbles], [0, 100, 200, true]);
});

test("a flick glides at the finger's speed per frame", () => {
  assert.strictEqual(T.glideSpeed(32, 16), 32); // 2px/ms is 32px a frame
  assert.strictEqual(T.glideSpeed(8, 64), 2);
  assert.strictEqual(T.glideSpeed(5, 0), 80); // no time passed: counted as 1ms, never a divide by zero
});

test("a finger that rested before lifting does not glide; a flick does", () => {
  // A phone sends no touchmove while the finger holds still, so the rest only
  // shows as time between the last move and the lift.
  assert.strictEqual(T.liftSpeed(32, 300), 0); // dragged fast, held, lifted: stays put
  assert.strictEqual(T.liftSpeed(32, T.REST_MS + 1), 0);
  assert.strictEqual(T.liftSpeed(32, T.REST_MS), 32); // lifted mid-flick: glides
  assert.strictEqual(T.liftSpeed(-20, 10), -20);
});
