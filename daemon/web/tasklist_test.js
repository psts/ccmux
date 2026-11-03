// Rules of tasklist.js: how the task queue reads on screen.
// Run: node --test daemon/web/tasklist_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const L = require("./tasklist.js");

const tasks = [
  { id: 1, repo: "backend", status: "done" },
  { id: 2, repo: "admin", status: "waiting", afterId: 1 },
  { id: 3, repo: "admin", status: "waiting" },
  { id: 4, repo: "admin", status: "needs_you" },
  { id: 5, repo: "backend", status: "failed" },
  { id: 6, repo: "admin", status: "planning", afterId: 5 },
];

test("sections put what needs a person first and keep queue order", () => {
  const got = L.sections(tasks, "");
  assert.deepStrictEqual(got.map((s) => s.key), ["needs_you", "waiting", "planning", "failed", "done"]);
  assert.deepStrictEqual(got[1].tasks.map((t) => t.id), [2, 3]);
});

test("a repo filter keeps only that repo's tasks", () => {
  const got = L.sections(tasks, "backend");
  assert.deepStrictEqual(got.map((s) => s.key), ["failed", "done"]);
  assert.deepStrictEqual(L.repos(tasks), ["admin", "backend"]);
});

test("the summary counts running, waiting and done, and alerts on what needs a person", () => {
  assert.deepStrictEqual(L.summary({ running: 1, waiting: 2, done: 4, needs_you: 1, ready_to_merge: 1 }),
    { text: "1 running · 2 waiting · 4 done", alert: 2 });
  assert.deepStrictEqual(L.summary(undefined), { text: "", alert: 0 });
});

test("waitsOn names the task it waits for and flags a dead one", () => {
  assert.strictEqual(L.waitsOn(tasks[2], tasks), null);
  assert.deepStrictEqual(L.waitsOn(tasks[1], tasks), { text: "after #1 backend (done)", blocked: false });
  assert.deepStrictEqual(L.waitsOn(tasks[5], tasks), { text: "blocked: #5 backend is failed", blocked: true });
  assert.deepStrictEqual(L.waitsOn({ afterId: 9 }, tasks), { text: "after #9 (removed)", blocked: true });
});
