// node --test threadpast_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const P = require("./threadpast.js");

test("an entry is its local time and its title cut short, the current one marked", () => {
  const at = new Date(2026, 10, 17, 9, 38).getTime();
  assert.equal(P.when(at), "2026-11-17 09:38");
  assert.equal(P.label({ updated: at, title: "  the   weekly\nbrief " }), "2026-11-17 09:38 · the weekly brief");
  assert.equal(P.label({ updated: at, title: "" , current: true }), "2026-11-17 09:38 (now)");
  const long = P.label({ updated: at, title: "x".repeat(60) });
  assert.equal(long, "2026-11-17 09:38 · " + "x".repeat(47) + "…");
  assert.match(P.barText({ updated: at }), /^An earlier conversation, from 2026-11-17 09:38\. Read only/);
});
