// node --test threadtodo_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const T = require("./threadtodo.js");

const item = (id, over = {}) => ({ id, repo: "/p/app", text: "item " + id, urgency: "low", status: "open", createdAt: id * 1000, ...over });
const ago = (since) => `at ${since}`;

test("ofRepo keeps the items of one list and nothing of another's", () => {
  const todos = [item(1), item(2, { repo: "/p/blog" }), item(3)];
  assert.deepEqual(T.ofRepo(todos, "/p/app").map((t) => t.id), [1, 3]);
  assert.deepEqual(T.ofRepo(todos, "/p/none"), []);
  assert.deepEqual(T.ofRepo(undefined, "/p/app"), []);
});

test("split reads unfinished items high first, newest first within an urgency, then the done ones latest first", () => {
  const list = [
    item(1, { urgency: "low" }), item(2, { urgency: "high" }), item(3, { urgency: "medium", status: "tasked" }),
    item(4, { urgency: "high" }), item(5, { urgency: "high", status: "done", doneAt: 50 }), item(6, { status: "done", doneAt: 90 }),
  ];
  const got = T.split(list);
  assert.deepEqual(got.open.map((t) => t.id), [4, 2, 3, 1], "a tasked item is still on the list, in its urgency's place");
  assert.deepEqual(got.done.map((t) => t.id), [6, 5]);
  assert.deepEqual(list.map((t) => t.id), [1, 2, 3, 4, 5, 6], "the list it was given is left as it was");
  assert.deepEqual(T.split(undefined), { open: [], done: [] });
});

test("summary counts what waits for someone, names the highest urgency there, and leaves out what is done", () => {
  const got = T.summary([
    item(1, { urgency: "low" }), item(2, { urgency: "medium" }), item(3, { urgency: "medium" }),
    item(4, { urgency: "high", status: "tasked" }), item(5, { urgency: "high", status: "done" }),
    item(6, { urgency: "high", status: "planning" }), item(7, { status: "planning" }),
  ]);
  assert.deepEqual(got, { open: 3, planning: 2, tasked: 1, top: "medium", text: "2 medium · 1 low · 2 planning · 1 tasked" },
    "a high item that the planner or a task already has does not make the row high");
  assert.deepEqual(T.summary([item(1, { urgency: "high" })]), { open: 1, planning: 0, tasked: 0, top: "high", text: "1 high" });
  assert.deepEqual(T.summary([item(1, { status: "planning" })]), { open: 0, planning: 1, tasked: 0, top: "", text: "1 planning" },
    "an item in the planning line is counted, like one a task has");
  assert.deepEqual(T.summary([item(1, { status: "done" })]), { open: 0, planning: 0, tasked: 0, top: "", text: "" });
  assert.deepEqual(T.summary(undefined), { open: 0, planning: 0, tasked: 0, top: "", text: "" });
});

test("chip says where an item someone has taken is, and nothing for the rest", () => {
  assert.equal(T.chip(item(1, { status: "planning" })), "planning");
  assert.equal(T.chip(item(1, { status: "tasked", taskId: 41 })), "task #41");
  assert.equal(T.chip(item(1, { status: "tasked" })), "sent to the repo's session to file", "sent before there were planners");
  assert.equal(T.chip(item(1)), "");
  assert.equal(T.chip(item(1, { status: "done", taskId: 41 })), "", "a done item's task is history");
});

test("plannerOf is the planner of the list's repo, in whichever of its sessions it runs", () => {
  const repos = { ws1: "/p/app", ws1b: "/p/app", ws2: "/p/blog" };
  const sessions = [
    { id: "lead1", workspaceId: "ws1", kind: "lead", state: "running" },
    { id: "old", workspaceId: "ws1", kind: "planner", state: "done" },
    { id: "plan1", workspaceId: "ws1b", kind: "planner", state: "asleep" },
    { id: "plan2", workspaceId: "ws2", kind: "planner", state: "running" },
  ];
  assert.equal(T.plannerOf(sessions, repos, "/p/app").id, "plan1", "not the lead, not one that ended, and by the list's key");
  assert.equal(T.plannerOf(sessions, repos, "/p/blog").id, "plan2");
  assert.equal(T.plannerOf(sessions, repos, "/p/none"), null);
  assert.equal(T.plannerOf(undefined, undefined, "/p/app"), null);
});

test("actions are the buttons each kind of item gets", () => {
  assert.deepEqual(T.actions(item(1)), ["task", "done", "delete"]);
  assert.deepEqual(T.actions(item(1, { status: "planning" })), ["again", "reopen", "done", "delete"], "in the planner's line: send it again, or put it back");
  assert.deepEqual(T.actions(item(1, { status: "tasked" })), ["again", "done", "delete"], "sent, nothing filed yet: it can be sent again");
  assert.deepEqual(T.actions(item(1, { status: "tasked", taskId: 41 })), ["done", "delete"], "a task has it: no second task");
  assert.deepEqual(T.actions(item(1, { status: "done" })), ["reopen", "delete"]);
  assert.deepEqual(["task", "again", "done", "delete", "reopen"].map(T.label), ["Make task", "Send again", "Done", "Delete", "Put back"]);
});

test("byline and doneLine say who and when, and a done item's note", () => {
  assert.equal(T.byline(item(1, { addedBy: "app-task-7" }), 0, ago), "added by app-task-7 · at 1000");
  assert.equal(T.byline(item(1), 0, ago), "added by someone · at 1000");
  assert.equal(T.doneLine(item(1, { doneBy: "task #41", doneAt: 7000 }), 0, ago), "done by task #41 · at 7000");
  assert.equal(T.doneLine(item(1, { doneBy: "Pat", doneAt: 7000, note: "fixed in 1a2b3c" }), 0, ago), "done by Pat · at 7000 · fixed in 1a2b3c");
});
