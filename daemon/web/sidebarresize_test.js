// Rules of sidebarresize.js: how wide a drag leaves the left menu.
// Run: node --test daemon/web/sidebarresize_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const R = require("./sidebarresize.js");

test("a width stays between the narrowest and widest the menu allows", () => {
  assert.strictEqual(R.clampWidth(300, 1600), 300);
  assert.strictEqual(R.clampWidth(40, 1600), R.MIN);
  assert.strictEqual(R.clampWidth(900, 1600), R.MAX);
});

test("the menu never takes more than half a narrow window", () => {
  assert.strictEqual(R.clampWidth(450, 800), 400);
  assert.strictEqual(R.clampWidth(300, 800), 300);
});

test("a stored width that is missing or junk reads as none", () => {
  assert.strictEqual(R.storedWidth(null, 1600), null);
  assert.strictEqual(R.storedWidth("wide", 1600), null);
  assert.strictEqual(R.storedWidth("320", 1600), 320);
  assert.strictEqual(R.storedWidth("9000", 1600), R.MAX);
});
