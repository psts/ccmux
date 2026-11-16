import { test } from "node:test";
import assert from "node:assert/strict";
import { RunnerState } from "./runnerstate.mjs";
import { RunnerTurns } from "./runnerturns.mjs";

function turns(data) {
  const state = new RunnerState(data);
  return { state, t: new RunnerTurns(state) };
}

test("a prompt queued behind a running turn keeps the session busy after the first result", () => {
  const { state, t } = turns();
  t.sent("p1", "fix it");
  t.sent("p2", "and commit");
  state.markDelivered(["p1", "p2"]);
  t.stamped(["p1"]);
  assert.equal(t.result(), true);
  assert.equal(t.busy(), true, "p2 is still open");
  assert.deepEqual(state.open.map((p) => p.id), ["p2"]);
  t.stamped(["p2"]);
  t.result();
  assert.equal(t.busy(), false);
});

test("prompts folded into one turn all close with its result", () => {
  const { state, t } = turns();
  t.sent("p1", "a");
  t.sent("p2", "b");
  t.stamped(["p1", "p2"]);
  t.result();
  assert.equal(state.open.length, 0);
});

test("a turn that names no prompt closes the oldest one Claude has, not one it never got", () => {
  const { state, t } = turns();
  t.sent("p0", "never arrived");
  t.sent("p1", "arrived");
  state.markDelivered(["p1"]);
  t.result();
  assert.deepEqual(state.open.map((p) => p.id), ["p0"]);
});

test("a crash during the second prompt still gets the notice, naming it", () => {
  const { t } = turns({ open: [{ id: "p2", text: "and commit" }], delivered: ["p1", "p2"] });
  const n = t.notice();
  assert.match(n, /cut off/);
  assert.match(n, /and commit/);
  assert.equal(t.busy(), false, "the notice takes over from the cut-off prompt");
  assert.equal(t.notice(), "", "said once");
});

test("an open prompt Claude never got draws no notice: the daemon resends it", () => {
  const { state, t } = turns({ open: [{ id: "p3", text: "later" }], delivered: [] });
  assert.equal(t.notice(), "");
  assert.deepEqual(state.open.map((p) => p.id), ["p3"]);
});

test("a prompt sent twice is open once", () => {
  const { state, t } = turns();
  t.sent("p1", "x");
  t.sent("p1", "x");
  assert.equal(state.open.length, 1);
});
