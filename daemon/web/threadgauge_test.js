// node --test threadgauge_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const m = require("./threadgauge.js");

test("the gauge: how full the context is and how long the prompt cache has left", () => {
  const min = 60000, at = 1000000;
  const r = { endedAt: at, cacheTtlMs: 60 * min, context: { percent: 42, used: 84000, max: 200000 } };
  const g = m.gauge(r, at + 14 * min);
  assert.equal(g.text, "42% · cache 46 min");
  assert.equal(g.percent, 42, "the ring's share");
  assert.equal(m.gauge({ endedAt: at, cacheTtlMs: 60 * min }, at).percent, null, "no context number, no ring");
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

test("while a turn runs the cache is in use, so it shows live, not a countdown or cold", () => {
  const r = { endedAt: 1000, cacheTtlMs: 300000, context: { percent: 42, used: 84000, max: 200000 } };
  const g = m.gauge(r, 1000 + 9 * 60000, true);
  assert.deepEqual([g.text, g.cache], ["42% · cache live", ""]);
  assert.match(g.title, /in use by the running turn/);
});

test("a message on a cold cache is asked about, by the gauge's own rule, unless the conversation is small", () => {
  const min = 60000, at = 1000000;
  const r = { endedAt: at, cacheTtlMs: 60 * min, context: { percent: 42, used: 84000, max: 200000 } };
  assert.equal(m.coldAsk(r, at + 59 * min), null, "warm: no question");
  const a = m.coldAsk(r, at + 72 * min);
  assert.deepEqual(a, { used: 84000, max: 200000, percent: 42, coldFor: "12 min" });
  assert.match(m.coldText(a), /went cold 12 min ago\. Continuing re-reads 84k tokens \(42% of the context window\) at full price/);
  assert.equal(m.coldAsk(r, at + 72 * min, true), null, "a running turn keeps it warm");
  assert.equal(m.coldAsk({ ...r, context: { percent: 5, used: 10000, max: 200000 } }, at + 72 * min), null, "a small conversation costs little: no question");
  const unknown = m.coldAsk({ endedAt: at, cacheTtlMs: 60 * min }, at + 72 * min);
  assert.equal(unknown.max, 0, "cold with no size known still asks");
  assert.match(m.coldText(unknown), /re-reads the whole conversation/);
  assert.equal(m.coldAsk(null, at), null);
  assert.equal(m.coldAsk({ endedAt: 0, cacheTtlMs: 0 }, at), null, "nothing known, nothing asked");
});
