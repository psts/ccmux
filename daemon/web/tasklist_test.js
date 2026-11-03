// Rules of tasklist.js: how the task queue reads on screen.
// Run: node --test daemon/web/tasklist_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const L = require("./tasklist.js");

const part = (repo, status) => ({ repo, status: status || "pending" });
const tasks = [
  { id: 1, title: "Export route", status: "done", parts: [part("backend", "ready")] },
  { id: 2, title: "Export", status: "waiting", afterId: 1, parts: [part("app"), part("backend")] },
  { id: 3, title: "Dark mode", status: "waiting", parts: [part("app")] },
  { id: 4, title: "Login", status: "needs_you", parts: [part("app", "needs_you")] },
  { id: 5, title: "Search", status: "cancelled", parts: [part("backend")] },
  { id: 6, title: "Settings", status: "planning", afterId: 5, parts: [part("admin")] },
];

test("sections put what needs a person first and keep queue order", () => {
  const got = L.sections(tasks, "");
  assert.deepStrictEqual(got.map((s) => s.key), ["needs_you", "waiting", "planning", "done", "cancelled"]);
  assert.deepStrictEqual(got[1].tasks.map((t) => t.id), [2, 3]);
});

test("a repo filter keeps the features with a part in that repo", () => {
  const got = L.sections(tasks, "backend");
  assert.deepStrictEqual(got.map((s) => s.key), ["waiting", "done", "cancelled"]);
  assert.deepStrictEqual(got[0].tasks.map((t) => t.id), [2]);
  assert.deepStrictEqual(L.repos(tasks), ["admin", "app", "backend"]);
});

test("the summary counts running, waiting and done, and alerts on what needs a person", () => {
  assert.deepStrictEqual(L.summary({ running: 1, waiting: 2, done: 4, needs_you: 1, ready_to_merge: 1 }),
    { text: "1 running · 2 waiting · 4 done", alert: 2 });
  assert.deepStrictEqual(L.summary(undefined), { text: "", alert: 0 });
});

test("waitsOn names the task it waits for and flags a dead one", () => {
  assert.strictEqual(L.waitsOn(tasks[2], tasks), null);
  assert.deepStrictEqual(L.waitsOn(tasks[1], tasks), { text: "after #1 Export route (done)", blocked: false });
  assert.deepStrictEqual(L.waitsOn(tasks[5], tasks), { text: "blocked: #5 Search is cancelled", blocked: true });
  assert.deepStrictEqual(L.waitsOn({ afterId: 9 }, tasks), { text: "after #9 (removed)", blocked: true });
});

test("a part's status reads as words", () => {
  assert.strictEqual(L.partWord("ready"), "done");
  assert.strictEqual(L.partWord("odd"), "odd");
});
