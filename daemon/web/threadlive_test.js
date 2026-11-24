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

test("a background task reads as its own words, its kind and agent apart", () => {
  assert.deepEqual(L.taskParts({ id: "b1", type: "local_bash", description: "npm run dev" }), { kind: "Shell", name: "npm run dev", agent: "" });
  assert.deepEqual(L.taskParts({ id: "a1", type: "local_agent", agent: "qa:code-reviewer", description: "Code review" }), { kind: "Agent", name: "Code review", agent: "qa:code-reviewer" });
  assert.deepEqual(L.taskParts({ id: "x9", type: "something_new", description: "" }), { kind: "Task", name: "x9", agent: "" });
});

test("how long a task has run, from when the runner first saw it", () => {
  const now = 10 * 3600000;
  assert.equal(L.ranFor(undefined, now), "", "a runner from before kept no time");
  assert.equal(L.ranFor(now - 20000, now), "<1m");
  assert.equal(L.ranFor(now - 12 * 60000, now), "12m");
  assert.equal(L.ranFor(now - 65 * 60000, now), "1h 5m");
  assert.equal(L.ranFor(now - 2 * 3600000, now), "2h");
  assert.equal(L.ranFor(now + 5000, now), "<1m", "a clock a little ahead is not negative");
});

test("the folded tray names how many run and the first two, then how many more", () => {
  const t = (description) => ({ id: description, type: "local_agent", description });
  assert.equal(L.trayLine([t("Code review"), t("Silent-failure review"), t("Test coverage")]), "Code review, Silent-failure review, +1");
  assert.equal(L.trayLine([t("Code review"), t("Silent-failure review")]), "Code review, Silent-failure review");
});

test("the modes offered are the ones the runner switches to (claudeserve/sessionself.mjs)", () => {
  const src = require("node:fs").readFileSync(require("node:path").join(__dirname, "../internal/agent/claudeserve/sessionself.mjs"), "utf8");
  const runner = JSON.parse(/export const MODES = (\[[^\]]*\]);/.exec(src)[1]);
  assert.deepEqual(L.MODES.map((m) => m.value), runner, "the two lists must not drift apart");
});

test("the efforts offered are the ones the session's model takes, the picked one always there", () => {
  const info = { model: "claude-haiku-5-5", models: [{ value: "haiku", model: "claude-haiku-5-5", efforts: ["low", "high"] }] };
  assert.deepEqual(L.effortOptions(info, "").map((e) => e.value), ["", "low", "high"]);
  assert.deepEqual(L.effortOptions(info, "max").map((e) => e.value), ["", "low", "high", "max"], "what the row names stays");
  assert.deepEqual(L.effortOptions({}, "").map((e) => e.value), L.EFFORTS.map((e) => e.value), "a runner that did not say: all");
});

test("the page's efforts are the runner's and the daemon's (sessionself.mjs, api/sessioneffort.go)", () => {
  const fs = require("node:fs"), path = require("node:path");
  const runner = JSON.parse(/export const EFFORTS = (\[[^\]]*\]);/.exec(fs.readFileSync(path.join(__dirname, "../internal/agent/claudeserve/sessionself.mjs"), "utf8"))[1]);
  const daemon = JSON.parse("[" + /var efforts = \[\]string\{([^}]*)\}/.exec(fs.readFileSync(path.join(__dirname, "../internal/api/sessioneffort.go"), "utf8"))[1] + "]");
  assert.deepEqual(L.EFFORTS.map((e) => e.value), runner);
  assert.deepEqual(runner, daemon, "the three lists must not drift apart");
});
