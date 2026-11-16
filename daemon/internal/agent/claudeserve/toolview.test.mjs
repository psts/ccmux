// node --test toolview.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { toolDiff, todosAfter, diffCap } from "./toolview.mjs";

test("an edit's diff is Claude Code's own patch when the result carries one", () => {
  // The shape a live session's tool_use_result has for Edit (SDK 0.3.311,
  // Claude Code 2.1.325, recorded 2026-11-16).
  const result = { filePath: "/r/a.txt", oldString: "world", newString: "there", structuredPatch: [{ oldStart: 1, oldLines: 2, newStart: 1, newLines: 2, lines: [" hello", "-world", "+there"] }] };
  const d = toolDiff("Edit", { file_path: "/r/a.txt", old_string: "world", new_string: "there" }, result);
  assert.deepEqual(d, { path: "/r/a.txt", added: 1, removed: 1, hunks: [{ oldStart: 1, newStart: 1, lines: [" hello", "-world", "+there"] }] });
});

test("with no result the diff is built from what the call asked for", () => {
  const d = toolDiff("Edit", { file_path: "/r/a.go", old_string: "a\nb", new_string: "a\nc\nd" });
  assert.deepEqual(d.hunks, [{ oldStart: 0, newStart: 0, lines: ["-a", "-b", "+a", "+c", "+d"] }]);
  assert.deepEqual([d.added, d.removed], [3, 2]);
});

test("a new file is all added lines, and its empty patch does not hide them", () => {
  const created = { type: "create", filePath: "/r/n.txt", content: "one\ntwo\n", structuredPatch: [], originalFile: null };
  const d = toolDiff("Write", { file_path: "/r/n.txt", content: "one\ntwo\n" }, created);
  assert.deepEqual(d, { path: "/r/n.txt", added: 2, removed: 0, hunks: [{ oldStart: 0, newStart: 1, lines: ["+one", "+two"] }] });
  assert.equal(toolDiff("Write", { file_path: "/r/empty", content: "" }).added, 0);
});

test("a write over an existing file shows the patch, not the whole file", () => {
  const updated = { type: "update", structuredPatch: [{ oldStart: 3, oldLines: 1, newStart: 3, newLines: 1, lines: ["-x", "+y"] }] };
  const d = toolDiff("Write", { file_path: "/r/big.txt", content: "…many lines…\n" }, updated);
  assert.deepEqual(d.hunks, [{ oldStart: 3, newStart: 3, lines: ["-x", "+y"] }]);
});

test("each edit of a multi-edit is its own hunk", () => {
  const d = toolDiff("MultiEdit", { file_path: "/r/m.go", edits: [{ old_string: "a", new_string: "b" }, { old_string: "c", new_string: "" }] });
  assert.equal(d.hunks.length, 2);
  assert.deepEqual([d.added, d.removed], [1, 2]);
});

test("tools that write no file, and calls with no path yet, have no diff", () => {
  assert.equal(toolDiff("Bash", { command: "ls" }), null);
  assert.equal(toolDiff("Read", { file_path: "/r/a" }), null);
  assert.equal(toolDiff("Edit", {}), null);
  assert.equal(toolDiff("Edit", null), null);
});

test("a long diff is cut on the wire and keeps its true counts", () => {
  const content = Array.from({ length: diffCap + 50 }, (_, i) => "l" + i).join("\n");
  const d = toolDiff("Write", { file_path: "/r/long", content });
  assert.equal(d.added, diffCap + 50);
  assert.equal(d.hunks[0].lines.length, diffCap);
  assert.equal(d.truncated, true);
  assert.equal(toolDiff("Write", { file_path: "/r/short", content: "a" }).truncated, undefined);
});

test("TodoWrite replaces the list", () => {
  const next = todosAfter([{ id: "1", text: "old", status: "pending" }], "TodoWrite", { todos: [{ content: "Write the test", status: "completed", activeForm: "Writing" }, { content: "Ship", status: "in_progress" }, { content: "Odd", status: "weird" }] });
  assert.deepEqual(next, [{ id: "1", text: "Write the test", status: "completed" }, { id: "2", text: "Ship", status: "in_progress" }, { id: "3", text: "Odd", status: "pending" }]);
});

test("the task tools add, change and delete one item", () => {
  let list = todosAfter([], "TaskCreate", { subject: "Backend route" }, { task: { id: "7", subject: "Backend route" } });
  list = todosAfter(list, "TaskCreate", { subject: "Admin button" });
  assert.deepEqual(list.map((t) => t.id), ["7", "2"], "the result's id when there is one, else its place");
  list = todosAfter(list, "TaskUpdate", { taskId: "7", status: "completed" });
  list = todosAfter(list, "TaskUpdate", { taskId: "2", subject: "Admin table button" });
  assert.deepEqual(list, [{ id: "7", text: "Backend route", status: "completed" }, { id: "2", text: "Admin table button", status: "pending" }]);
  assert.deepEqual(todosAfter(list, "TaskUpdate", { taskId: "7", status: "deleted" }).map((t) => t.id), ["2"]);
});

test("any other tool leaves the list alone", () => {
  assert.equal(todosAfter([{ id: "1", text: "x", status: "pending" }], "Bash", { command: "ls" }), null);
  assert.equal(todosAfter([], "TodoWrite", null), null);
});
