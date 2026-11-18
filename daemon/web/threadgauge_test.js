// node --test threadgauge_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const m = require("./threadgauge.js");

test("the gauge: how full the context is and how long the prompt cache has left", () => {
  const min = 60000, at = 1000000;
  const r = { endedAt: at, cacheTtlMs: 60 * min, context: { percent: 42, used: 84000, max: 200000 } };
  const g = m.gauge(r, at + 14 * min);
  assert.equal(g.text, "42% · cache 46 min");
  assert.deepEqual([g.ctx, g.cache], ["", ""]);
  assert.match(g.title, /84k of 200k tokens/);
  assert.match(g.title, /last reply 14 min ago/);
  assert.equal(m.gauge({ ...r, context: { percent: 85, used: 170000, max: 200000 } }, at).ctx, "warn", "near full is orange");
  const late = m.gauge(r, at + 57 * min);
  assert.deepEqual([late.text, late.cache], ["42% · cache 3 min", "warn"]);
  assert.equal(m.gauge(r, at + 60 * min - 20000).text, "42% · cache <1 min");
  const cold = m.gauge(r, at + 72 * min);
  assert.deepEqual([cold.text, cold.cache], ["42% · cache cold", "cold"]);
  assert.match(cold.title, /expired 12 min ago/);
  const five = m.gauge({ ...r, cacheTtlMs: 5 * min }, at + 4.5 * min);
  assert.deepEqual([five.text, five.cache], ["42% · cache <1 min", "warn"]);
  assert.equal(m.gauge({ endedAt: at, cacheTtlMs: 0 }, at), null, "nothing known, nothing shown");
  assert.equal(m.gauge(null, at), null);
  assert.equal(m.gauge({ endedAt: at, cacheTtlMs: 60 * min }, at + min).text, "cache 59 min", "a cache with no context number yet");
});
