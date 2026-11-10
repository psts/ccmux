// Rules of tasksettings.js: the cap across windows on the Settings page.
// Run: node --test daemon/web/tasksettings_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const T = require("./tasksettings.js");

test("the cap picker offers no limit and 1 to 6, marking the daemon's number", () => {
  assert.deepStrictEqual(T.capChoices(0).map((c) => [c.value, c.label, c.selected]), [
    [0, "No limit", true], [1, "1 at a time", false], [2, "2 at a time", false], [3, "3 at a time", false],
    [4, "4 at a time", false], [5, "5 at a time", false], [6, "6 at a time", false]]);
  assert.deepStrictEqual(T.capChoices(4).filter((c) => c.selected).map((c) => c.value), [4]);
});

test("a cap the daemon did not send gets no picker", () => {
  assert.deepStrictEqual(T.capChoices(undefined), []);
  assert.deepStrictEqual(T.capChoices(null), []);
});
