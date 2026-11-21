import { test } from "node:test";
import assert from "node:assert/strict";
import { RunnerState, loadState, saveState, EventLog } from "./runnerstate.mjs";

test("delivered ids are kept once and capped", () => {
  const s = new RunnerState();
  assert.equal(s.markDelivered(["a", "b"]), true);
  assert.equal(s.markDelivered(["a"]), false);
  assert.ok(s.isDelivered("b"));
  s.markDelivered(Array.from({ length: 600 }, (_, i) => "x" + i));
  assert.equal(s.delivered.length, 500);
  assert.ok(s.isDelivered("x599"));
  assert.ok(!s.isDelivered("a"));
});

test("a missing state file is a fresh session, a broken one is said", () => {
  const warns = [];
  const enoent = () => { const e = new Error("no"); e.code = "ENOENT"; throw e; };
  assert.deepEqual(loadState("/s.json", enoent, (w) => warns.push(w)).open, []);
  assert.equal(warns.length, 0);
  assert.deepEqual(loadState("/s.json", () => "{oops", (w) => warns.push(w)).open, []);
  assert.equal(warns.length, 1);
});

test("state round-trips through a temporary file", () => {
  const files = {};
  const fs = { writeFile: (p, t) => { files[p] = t; }, rename: (a, b) => { files[b] = files[a]; delete files[a]; } };
  const s = new RunnerState();
  s.open.push({ id: "p1", text: "hi" });
  s.markDelivered(["p1"]);
  saveState("/s.json", s, fs);
  assert.deepEqual(Object.keys(files), ["/s.json"]);
  const back = loadState("/s.json", (p) => files[p], () => {});
  assert.deepEqual(back.open, [{ id: "p1", text: "hi" }]);
  assert.ok(back.isDelivered("p1"));
});

test("the event log replays after n and reports a gap", () => {
  const log = new EventLog(3);
  for (const n of [1, 2, 3, 4, 5]) log.push({ n });
  assert.deepEqual(log.after(3).events.map((x) => x.seq), [4, 5]);
  assert.equal(log.after(3).gap, false);
  assert.equal(log.after(1).gap, true);
  assert.equal(log.after(0).gap, false);
  assert.deepEqual(log.after(0).events.map((x) => x.seq), [3, 4, 5]);
});

test("the last bill is kept for its own conversation only, and round-trips", () => {
  const s = new RunnerState();
  s.keepResult("c1", { endedAt: 5, cacheTtlMs: 3600000, context: { used: 10, max: 100, percent: 10 } });
  assert.deepEqual(s.resultFor("c1"), { endedAt: 5, cacheTtlMs: 3600000, context: { used: 10, max: 100, percent: 10 } });
  assert.equal(s.resultFor("c2"), null, "a clean start's new conversation has no bill yet");
  const back = new RunnerState(JSON.parse(JSON.stringify(s)));
  assert.equal(back.resultFor("c1").endedAt, 5);
  assert.equal(new RunnerState({ lastResult: { endedAt: 1 } }).resultFor(undefined), null, "one with no conversation is dropped");
  assert.equal("lastResult" in new RunnerState().toJSON(), false);
});
