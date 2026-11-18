// Rules of tasklist.js: how the task queue reads on screen.
// Run: node --test daemon/web/tasklist_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const L = require("./tasklist.js");

const part = (repo, status) => ({ repo, status: status || "pending" });
const tasks = [
  { id: 1, title: "Export route", status: "done", parts: [part("backend", "ready")] },
  { id: 2, title: "Export", status: "waiting", afterIds: [1], parts: [part("app"), part("backend")] },
  { id: 3, title: "Dark mode", status: "waiting", parts: [part("app")] },
  { id: 4, title: "Login", status: "needs_you", parts: [part("app", "needs_you")] },
  { id: 5, title: "Search", status: "cancelled", parts: [part("backend")] },
  { id: 6, title: "Settings", status: "planning", afterIds: [5], parts: [part("admin")] },
  { id: 7, title: "Billing", status: "merge_failed", parts: [part("backend", "ready")] },
];

test("sections put what needs a person first and keep queue order", () => {
  const got = L.sections(tasks, "");
  assert.deepStrictEqual(got.map((s) => s.key), ["needs_you", "merge_failed", "waiting", "planning", "done", "cancelled"]);
  assert.deepStrictEqual(got[2].tasks.map((t) => t.id), [2, 3]);
});

test("a repo filter keeps the features with a part in that repo", () => {
  const got = L.sections(tasks, "backend");
  assert.deepStrictEqual(got.map((s) => s.key), ["merge_failed", "waiting", "done", "cancelled"]);
  assert.deepStrictEqual(got[1].tasks.map((t) => t.id), [2]);
  assert.deepStrictEqual(L.repos(tasks), ["admin", "app", "backend"]);
});

test("the summary counts running, merging, waiting and done, and alerts on what needs a person", () => {
  assert.deepStrictEqual(L.summary({ running: 1, waiting: 2, done: 4, needs_you: 1, merge_failed: 1, ready_to_merge: 2, merging: 1 }),
    { text: "1 running · 3 merging · 2 waiting · 4 done", alert: 2 });
  assert.deepStrictEqual(L.summary({ waiting: 2, merging: 1 }), { text: "1 merging · 2 waiting", alert: 0 });
  assert.deepStrictEqual(L.summary(undefined), { text: "", alert: 0 });
});

test("waitsOn names the task it waits for and flags a dead one", () => {
  assert.strictEqual(L.waitsOn(tasks[2], tasks), null);
  assert.strictEqual(L.waitsOn({ afterIds: [] }, tasks), null);
  assert.deepStrictEqual(L.waitsOn(tasks[1], tasks), { text: "after #1 Export route (done)", blocked: false });
  assert.deepStrictEqual(L.waitsOn(tasks[5], tasks), { text: "blocked: #5 Search is cancelled", blocked: true });
  assert.deepStrictEqual(L.waitsOn({ afterIds: [9] }, tasks), { text: "blocked: #9 (removed)", blocked: true });
});

test("a task that starts after several names them all, and only the dead ones once one is", () => {
  assert.deepStrictEqual(L.waitsOn({ afterIds: [1, 3, 4] }, tasks),
    { text: "after #1 Export route (done), #3 Dark mode, #4 Login", blocked: false });
  assert.deepStrictEqual(L.waitsOn({ afterIds: [3, 5, 9] }, tasks),
    { text: "blocked: #5 Search is cancelled, #9 (removed)", blocked: true });
});

test("a part's status reads as words", () => {
  assert.strictEqual(L.partWord("ready"), "done");
  assert.strictEqual(L.partWord("odd"), "odd");
});

test("a failed merge is offered again by name", () => {
  assert.strictEqual(L.moveLabel("ready_to_merge"), "Try merge again");
  assert.strictEqual(L.moveLabel("done"), "Mark merged");
});

test("the slots picker offers Paused to 3 and marks the window's number", () => {
  assert.deepStrictEqual(L.slotChoices(0).map((c) => [c.value, c.label, c.selected]),
    [[0, "Paused", true], [1, "1 at a time", false], [2, "2 at a time", false], [3, "3 at a time", false]]);
  assert.deepStrictEqual(L.slotChoices(2).filter((c) => c.selected).map((c) => c.value), [2]);
});

test("a window with no number from the daemon gets no picker", () => {
  assert.deepStrictEqual(L.slotChoices(undefined), []);
  assert.deepStrictEqual(L.slotChoices(null), []);
});

test("a part says what it is built with, and one from before says nothing", () => {
  assert.strictEqual(L.builtWith({ repo: "app", model: "opus", effort: "medium" }), "opus · medium effort");
  assert.strictEqual(L.builtWith({ repo: "app", model: "fable", effort: "high" }), "fable · high effort");
  assert.strictEqual(L.builtWith({ repo: "app", model: "opus" }), "opus");
  assert.strictEqual(L.builtWith(part("app")), "");
});

test("done lists the latest finished first", () => {
  const ts = [
    { id: 1, status: "done", finishedAt: 100 },
    { id: 2, status: "done", finishedAt: 300 },
    { id: 3, status: "done", finishedAt: 200 },
    { id: 4, status: "waiting" },
  ];
  const done = L.sections(ts, "").find((s) => s.key === "done");
  assert.deepStrictEqual(done.tasks.map((t) => t.id), [2, 3, 1]);
});

test("waiting lists the next to start first: the daemon's ahead, then queue order", () => {
  // ahead is what the daemon sends (taskq.Ahead): 0 can start now, -1 holds
  // until a person acts and goes last.
  const ts = [
    { id: 10, status: "running", ahead: 0 },
    { id: 11, status: "waiting", ahead: 1 },
    { id: 12, status: "waiting", ahead: 2 },
    { id: 14, status: "waiting", ahead: -1 },
    { id: 13, status: "waiting", ahead: 1 },
    { id: 15, status: "waiting", ahead: 0 },
    { id: 16, status: "waiting", ahead: 0 },
    { id: 7, status: "done", finishedAt: 1, ahead: 0 },
  ];
  const waiting = L.sections(ts, "").find((s) => s.key === "waiting");
  assert.deepStrictEqual(waiting.tasks.map((t) => t.id), [15, 16, 11, 13, 12, 14]);
  const running = L.sections(ts, "").find((s) => s.key === "running");
  assert.deepStrictEqual(running.tasks.map((t) => t.id), [10], "other lists keep queue order");
});

test("the priority picker offers none and 1 to 9, on the number the window has", () => {
  const c = L.priorityChoices(1, undefined);
  assert.deepStrictEqual(c.map((x) => x.value), [0, 1, 2, 3, 4, 5, 6, 7, 8, 9]);
  assert.deepStrictEqual(c.filter((x) => x.selected).map((x) => x.label), ["No priority"]);
  assert.deepStrictEqual(L.priorityChoices(0, 2).filter((x) => x.selected).map((x) => x.label), ["Priority 2"]);
});

test("no task runner, no priority picker", () => {
  assert.deepStrictEqual(L.priorityChoices(undefined, 1), []);
  assert.deepStrictEqual(L.priorityChoices(null, undefined), []);
});

test("a repo lists the unfinished tasks it has a part in, in its window, needs-you first", () => {
  const p = (repoPath) => ({ repo: repoPath.split("/").pop(), repoPath });
  const ts = [
    { id: 1, windowId: "w", status: "waiting", parts: [p("/r/app")] },
    { id: 2, windowId: "w", status: "running", parts: [p("/r/blog"), p("/r/app")] }, // filed by blog, has a part in app
    { id: 3, windowId: "w", status: "done", parts: [p("/r/app")] },
    { id: 4, windowId: "w", status: "cancelled", parts: [p("/r/app")] },
    { id: 5, windowId: "other", status: "waiting", parts: [p("/r/app")] },
    { id: 6, windowId: "w", status: "needs_you", parts: [p("/r/app")] },
    { id: 7, windowId: "w", status: "planning", parts: [p("/r/blog")] },
  ];
  assert.deepStrictEqual(L.repoTasks(ts, "w", "/r/app").map((t) => t.id), [6, 2, 1]);
  assert.deepStrictEqual(L.repoTasks(ts, "w", "/r/blog").map((t) => t.id), [2, 7]);
  assert.deepStrictEqual(L.repoTasks(ts, "w", "/r/none"), []);
  assert.strictEqual(L.statusWord("needs_you"), "Needs you");
  assert.ok(L.needsPerson(ts[5]) && !L.needsPerson(ts[0]));
});

test("forYou lists the builders' lines, naming the repo only when there are several parts", () => {
  assert.deepStrictEqual(L.forYou({ parts: [{ repo: "app" }] }), []);
  assert.deepStrictEqual(L.forYou({ parts: [{ repo: "app", forYou: ["Prices change."] }] }), ["Prices change."]);
  assert.deepStrictEqual(L.forYou({ parts: [{ repo: "app", forYou: ["Email goes out."] }, { repo: "api" }, { repo: "web", forYou: ["New endpoint."] }] }),
    ["app: Email goes out.", "web: New endpoint."]);
});

test("preview is a note's first line, cut short", () => {
  assert.strictEqual(L.preview("\n  DECISIONS I MADE\n- one"), "DECISIONS I MADE");
  assert.strictEqual(L.preview(""), "");
  const long = L.preview("x".repeat(300));
  assert.ok(long.length === 120 && long.endsWith("…"), long);
});

test("reportHtml draws headings, lists and code, and escapes every text", () => {
  const esc = (s) => String(s).replace(/[<>&"']/g, (c) => ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&#39;" }[c]));
  const md = require("./threadmd.js");
  const html = L.reportHtml(md.parse("## Built\n- a `<b>` tag\n  - nested **bold**\n\n```go\nx := 1 < 2\n```\n[docs](https://e.x/\"a)"), esc);
  assert.ok(html.includes('<div class="md-h md-h2">Built</div>'), html);
  assert.ok(html.includes('<span class="md-item">a <code>&lt;b&gt;</code> tag</span>'), html);
  assert.ok(html.includes('margin-left:16px') && html.includes("<strong>bold</strong>"), html);
  assert.ok(html.includes('<pre class="md-code"><code>x := 1 &lt; 2</code></pre>'), html);
  assert.ok(!html.includes("<b>") && !html.includes('"a)'), html);
});
