// Rules and wiring of drawerswipe.js: which swipes open or close the session
// drawer on a phone, and that an edge swipe never reaches the terminal.
// Run: node --test daemon/web/drawerswipe_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const D = require("./drawerswipe.js");

test("a swipe in from the left edge opens; one to the left closes", () => {
  assert.strictEqual(D.gesture({ x: 5, y: 300 }, { x: 120, y: 310 }, false), "open");
  assert.strictEqual(D.gesture({ x: 200, y: 300 }, { x: 60, y: 305 }, true), "close");
});

test("everything else leaves the drawer alone", () => {
  assert.strictEqual(D.gesture({ x: 80, y: 300 }, { x: 250, y: 300 }, false), ""); // not from the edge
  assert.strictEqual(D.gesture({ x: 5, y: 300 }, { x: 40, y: 300 }, false), ""); // too short
  assert.strictEqual(D.gesture({ x: 5, y: 100 }, { x: 70, y: 400 }, false), ""); // mostly down: a scroll
  assert.strictEqual(D.gesture({ x: 120, y: 300 }, { x: 5, y: 300 }, false), ""); // left while closed
  assert.strictEqual(D.gesture({ x: 60, y: 300 }, { x: 200, y: 300 }, true), ""); // right while open
});

// A document stand-in: capture listeners on it, then the terminal's listener,
// in that order, stopping where a listener called stopPropagation.
function fakeDoc() {
  const doc = { handlers: {}, addEventListener(type, fn) { (this.handlers[type] ||= []).push(fn); } };
  doc.fire = (type, x, y, terminal) => {
    let stopped = false;
    const point = { clientX: x, clientY: y };
    const e = { touches: type === "touchend" ? [] : [point], changedTouches: [point], stopPropagation() { stopped = true; } };
    for (const fn of doc.handlers[type] || []) fn(e);
    if (!stopped && terminal) terminal(type);
  };
  return doc;
}

test("an edge swipe opens the drawer and never reaches the terminal", () => {
  const doc = fakeDoc();
  let open = false;
  const seen = [];
  D.attach(doc, { isMobile: () => true, isOpen: () => open, open: () => { open = true; }, close: () => { open = false; } });
  const term = (type) => seen.push(type);
  doc.fire("touchstart", 4, 300, term);
  doc.fire("touchmove", 60, 302, term);
  doc.fire("touchend", 130, 305, term);
  assert.strictEqual(open, true);
  assert.deepStrictEqual(seen, []);
  // A swipe left over the open drawer closes it. (The drawer covers the
  // terminal while open, so on a phone this touch never lands on it.)
  doc.fire("touchstart", 200, 300, term);
  doc.fire("touchend", 40, 300, term);
  assert.strictEqual(open, false);
});

test("a touch away from the edge is the terminal's, and a desktop has no drawer swipe", () => {
  const doc = fakeDoc();
  let opened = 0;
  let mobile = true;
  const seen = [];
  D.attach(doc, { isMobile: () => mobile, isOpen: () => false, open: () => { opened++; }, close() {} });
  const term = (type) => seen.push(type);
  doc.fire("touchstart", 150, 300, term);
  doc.fire("touchmove", 150, 200, term);
  doc.fire("touchend", 150, 150, term);
  assert.deepStrictEqual(seen, ["touchstart", "touchmove", "touchend"]);
  mobile = false;
  doc.fire("touchstart", 4, 300, term);
  doc.fire("touchend", 200, 300, term);
  assert.strictEqual(opened, 0);
});
