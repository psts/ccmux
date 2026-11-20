// node --test threadlive_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const L = require("./threadlive.js");

const clock = (ms) => new Date(ms).toISOString().slice(11, 16);

test("the usage warning says how close, which limit and when it resets; far from it, nothing", () => {
  assert.equal(L.limitLine(null, clock), "");
  assert.equal(L.limitLine({ status: "allowed" }, clock), "");
  assert.equal(L.limitLine({ status: "allowed_warning", type: "five_hour", utilization: 0.861, resetsAt: 1795200000 }, clock),
    "You are near your 5-hour usage limit (86% used). It resets at 18:40.");
  assert.equal(L.limitLine({ status: "rejected", type: "seven_day_opus", resetsAt: 1795200000000 }, clock),
    "You have reached your weekly Opus usage limit. It resets at 18:40.", "milliseconds read as such");
  assert.equal(L.limitLine({ status: "allowed_warning" }, clock), "You are near your usage limit.");
});

test("a background task's row names its kind and what it runs", () => {
  assert.equal(L.taskLabel({ id: "b1", type: "local_bash", description: "npm run dev" }), "Shell: npm run dev");
  assert.equal(L.taskLabel({ id: "a1", type: "local_agent", agent: "Explore", description: "map the API" }), "Agent (Explore): map the API");
  assert.equal(L.taskLabel({ id: "x9", type: "something_new", description: "" }), "Task: x9");
});

test("the modes offered are the ones the runner switches to (claudeserve/sessionself.mjs)", () => {
  const src = require("node:fs").readFileSync(require("node:path").join(__dirname, "../internal/agent/claudeserve/sessionself.mjs"), "utf8");
  const runner = JSON.parse(/export const MODES = (\[[^\]]*\]);/.exec(src)[1]);
  assert.deepEqual(L.MODES.map((m) => m.value), runner, "the two lists must not drift apart");
});
