// node --test threadwork_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const w = require("./threadwork.js");

const tool = (id, name, input, state = {}) => ({ id, messageID: id.split(".")[0], type: "tool", tool: name, state: { status: "completed", input, output: "", ...state } });
const text = (id, t) => ({ id, type: "text", text: t });
const assistant = (id, parts, info = {}) => ({ info: { id, role: "assistant", ...info }, parts });
const user = (id, t, info = {}, extra = []) => ({ info: { id, role: "user", ...info }, parts: [text(id + ".0", t), ...extra] });
const diff = (path, added, removed) => ({ path, added, removed, hunks: [] });

const turns = [
  user("u1", "Add CSV export", {}, [{ id: "u1.1", type: "file", mime: "image/png", name: "0123.png" }]),
  assistant("m1", [{ id: "m1.0", type: "reasoning", text: "hm" }, text("m1.1", "On it.")]),
  assistant("m2", [tool("m2.0", "Read", { file_path: "/p/app/src/a.ts" })]),
  assistant("m3", [tool("m3.0", "Edit", { file_path: "/p/app/src/a.ts" }, { diff: diff("/p/app/src/a.ts", 18, 2) })]),
  assistant("m4", [tool("m4.0", "Edit", { file_path: "/p/app/src/a.ts" }, { diff: diff("/p/app/src/a.ts", 1, 1) }), tool("m4.1", "Write", { file_path: "/p/app/src/b.test.ts" }, { diff: diff("/p/app/src/b.test.ts", 31, 0) })]),
  assistant("m5", [tool("m5.0", "Bash", { command: "npm test -- trades\n# second line" }, { output: "Tests: 42 passed, 42 total" })]),
  assistant("m6", [text("m6.0", ""), text("m6.1", "Done. One choice left:")]),
];

test("tool calls between two things said fold into one run", () => {
  const b = w.blocks(turns, "/p/app", "app");
  assert.deepEqual(b.map((x) => x.kind), ["user", "claude", "work", "claude"]);
  assert.deepEqual(b[0].files, [{ name: "0123.png", mime: "image/png" }]);
  const run = b[2];
  assert.equal(run.summary, "Edited 2 files in app, ran 1 command, read 1 file");
  assert.equal(run.outcome, "42 passed");
  assert.equal(run.status, "done");
  assert.deepEqual(run.steps.map((s) => [s.kind, s.what, s.note]), [
    ["Read", "src/a.ts", ""], ["Edit", "src/a.ts", "+18 −2"], ["Edit", "src/a.ts", "+1 −1"], ["Write", "src/b.test.ts", "+31"], ["Bash", "npm test -- trades", "42 passed"],
  ]);
  assert.equal(b[3].text, "Done. One choice left:", "empty text is not a thing said");
});

test("a run still going, and a run with a failed step, say so", () => {
  const going = w.summarize([tool("a.0", "Bash", { command: "make" }, { status: "running" })]);
  assert.deepEqual([going.summary, going.status, going.outcome], ["Ran 1 command", "running", ""]);
  const broke = w.summarize([tool("a.0", "Edit", { file_path: "/x" }, { status: "error" }), tool("a.1", "Bash", { command: "go test" }, { output: "ok 12 passed" })]);
  assert.deepEqual([broke.status, broke.outcome], ["error", "1 step failed"]);
  assert.equal(w.step(tool("a.0", "Edit", { file_path: "/x" }, { status: "error", diff: diff("/x", 1, 1) })).note, "failed");
  assert.equal(w.summarize([tool("a.0", "Grep", { pattern: "x" }), tool("a.1", "mcp__claude-peers__send_message", {})]).summary, "Searched 1 time, took 1 other step");
  assert.equal(w.summarize([tool("a.0", "AskUserQuestion", { questions: [] }), tool("a.1", "Write", { file_path: "/x" })], "app").summary, "Edited 1 file in app, asked you 1 question");
});

test("a command's tests are read only when it says so in the usual words", () => {
  assert.equal(w.outcome("42 passed"), "42 passed");
  assert.equal(w.outcome("Tests: 3 failed, 39 passed, 42 total"), "3 failed");
  assert.equal(w.outcome("0 failed, 12 passed"), "12 passed");
  assert.equal(w.outcome("ok  \tccmux.dev/ccmuxd/internal/api\t1.2s"), "");
  assert.equal(w.outcome(undefined), "");
});

test("a prompt from the bus, a line Claude Code wrote itself and a note are not the human's words", () => {
  const b = w.blocks([
    user("u1", "[claude-peers message from backend (id 1a2b3c4d)]\nThe endpoint is live.\n\nReply with send_message to_id=1a2b3c4d."),
    user("u2", "Stop hook feedback:\nrun the gate", { synthetic: true }),
    { info: { id: "n1", role: "note", level: "warning" }, parts: [text("n1.0", "The API did not answer.")] },
    user("u3", "ok, go on"),
  ], "/p/app", "app");
  assert.deepEqual(b.map((x) => x.kind), ["bus", "note", "note", "user"]);
  assert.deepEqual([b[0].from, b[0].text], ["backend", "The endpoint is live."]);
  assert.deepEqual([b[1].own, b[2].level, b[2].text], [true, "warning", "The API did not answer."]);
  assert.equal(w.busMessage("fix the bug"), null);
  const machine = w.blocks([user("u1", "[ccmux] This session was restarted while a turn was running, so that turn was cut off."), user("u2", "[Request interrupted by user]"), user("u3", "[ccmux is slow today] says the human")]);
  assert.deepEqual(machine.map((x) => x.kind), ["note", "note", "user"], "only the two lines a machine writes, by how they start");
});

test("a note or a prompt in the middle of tool work ends the run there", () => {
  const b = w.blocks([
    assistant("m1", [tool("m1.0", "Bash", { command: "a" })]),
    { info: { id: "n1", role: "note" }, parts: [text("n1.0", "Stop says: no")] },
    assistant("m2", [tool("m2.0", "Bash", { command: "b" })]),
  ]);
  assert.deepEqual(b.map((x) => x.kind), ["work", "note", "work"]);
});

test("a turn that failed shows its error after what it did", () => {
  const b = w.blocks([assistant("m1", [tool("m1.0", "Bash", { command: "a" })], { error: { name: "error", data: { message: "overloaded" } } })]);
  assert.deepEqual(b.map((x) => [x.kind, x.text]), [["work", undefined], ["error", "overloaded"]]);
});

test("a path outside the repo is written whole", () => {
  assert.equal(w.rel("/p/app/src/a.ts", "/p/app"), "src/a.ts");
  assert.equal(w.rel("/p/application/a.ts", "/p/app"), "/p/application/a.ts");
  assert.equal(w.rel("/etc/hosts", ""), "/etc/hosts");
  assert.equal(w.rel("/wt/app/thread-3/src/a.ts", ["/wt/app/thread-3", "/p/app"]), "src/a.ts", "a thread member's worktree is a root too");
  assert.equal(w.rel("/p/app/src/a.ts", ["/wt/app/thread-3", "/p/app"]), "src/a.ts");
  assert.equal(w.rel("/elsewhere/a.ts", ["/wt/app/thread-3", "/p/app"]), "/elsewhere/a.ts");
});

test("a thread member's paths are written without its repo's folder or its worktree, also once the worktree is gone from its row", () => {
  const edit = (path) => ({ parts: [{ type: "tool", state: { input: { file_path: path } } }] });
  const member = { threadId: 3, worktree: "/wt/app-b4/thread-3" };
  assert.deepEqual(w.threadRoots(member, "/p/app", []), ["/wt/app-b4/thread-3", "/p/app"]);
  // Merged: the row names no worktree, the conversation still does.
  const merged = w.threadRoots({ threadId: 3 }, "/p/app", [edit("/p/app/README.md"), edit("/wt/app-b4/thread-3/src/a.ts"), edit("/wt/app-b4/thread-3/src/b.ts")]);
  assert.deepEqual(merged, ["/wt/app-b4/thread-3", "/p/app"]);
  assert.equal(w.rel("/wt/app-b4/thread-3/src/a.ts", merged), "src/a.ts");
  // Another thread's worktree, and a session in no thread, add nothing.
  assert.deepEqual(w.threadRoots({ threadId: 3 }, "/p/app", [edit("/wt/app-b4/thread-30/src/a.ts")]), ["/p/app"]);
  assert.deepEqual(w.threadRoots({}, "/p/app", [edit("/wt/app-b4/thread-3/src/a.ts")]), ["/p/app"]);
  assert.deepEqual(w.threadRoots(null, "", []), []);
});
