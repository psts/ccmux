// node --test threadadvisor_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const A = require("./threadadvisor.js");

const info = (advisor, model = "claude-opus-5-5") => ({ model, advisor });
const values = (v) => v.options.map((o) => o.value);

test("no picker until the runner says which advisor it runs with", () => {
  assert.equal(A.advisorView({ advisor: "" }, { model: "claude-opus-5-5" }, "opus").known, false);
  assert.equal(A.advisorView({ advisor: "" }, null, "opus").known, false);
});

test("the settings' advisor is the first option, then the ones that rank at or above the model", () => {
  const v = A.advisorView({ advisor: "" }, info({ model: "fable", from: "settings", settings: "fable" }), "opus");
  assert.deepEqual(v.options, [{ value: "", label: "Settings (Fable)" }, { value: "fable", label: "Fable" }, { value: "opus", label: "Opus" }]);
  assert.equal(v.wanted, "");
  assert.equal(v.runningName, "Fable");
  assert.equal(v.pending, false);
  assert.deepEqual(values(A.advisorView({}, info({ model: "", from: "", settings: "" }), "claude-sonnet-5-5")), ["", "fable", "opus", "sonnet"]);
  assert.deepEqual(values(A.advisorView({}, info({ model: "", from: "", settings: "" }), "fable")), ["", "fable"]);
  assert.equal(A.advisorView({}, info({ model: "", from: "", settings: "" }), "opus").options[0].label, "Settings (none)");
});

test("a pick other than what the runner started with applies on the next restart", () => {
  const settings = { model: "fable", from: "settings", settings: "fable" };
  const picked = A.advisorView({ advisor: "opus" }, info(settings), "opus");
  assert.equal(picked.pending, true);
  assert.equal(picked.wantedName, "Opus");
  assert.equal(picked.runningName, "Fable");
  assert.equal(A.advisorView({ advisor: "opus" }, info({ model: "opus", from: "session", settings: "fable" }), "opus").pending, false, "it started with it");
  const back = A.advisorView({ advisor: "" }, info({ model: "opus", from: "session", settings: "fable" }), "opus");
  assert.equal(back.pending, true, "back to the settings is a change too");
  assert.equal(back.wantedName, "Fable");
});

test("an advisor the model outranks stays listed while the row asks for it", () => {
  const v = A.advisorView({ advisor: "sonnet" }, info({ model: "sonnet", from: "session", settings: "" }), "opus");
  assert.deepEqual(values(v), ["", "fable", "opus", "sonnet"]);
  assert.equal(v.wanted, "sonnet");
});

test("the restart bar names what applies and what the session keeps until then", () => {
  const mv = { wantedName: "Haiku", runningName: "Opus" }, av = { wantedName: "Opus", runningName: "Fable" };
  assert.deepEqual(A.restartBar(mv, av, true, false), { pending: true, what: "Haiku", keeps: "keeps running on Opus" });
  assert.deepEqual(A.restartBar(mv, av, false, true), { pending: true, what: "Advisor Opus", keeps: "keeps Fable as its advisor" });
  assert.equal(A.restartBar(mv, av, true, true).what, "Haiku and Advisor Opus");
  assert.equal(A.restartBar(mv, av, false, false).pending, false);
});
