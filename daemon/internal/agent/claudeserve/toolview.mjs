// What a tool call did, in the shapes the thread page draws: the diff of an
// Edit, Write or MultiEdit, and the to-do list a to-do tool leaves. Pure: no
// I/O and no SDK import (toolview.test.mjs).
//
// A diff comes from the tool's own result when the SDK hands one over (a
// live session's tool_use_result carries the patch Claude Code computed,
// with line numbers and context), and from the tool's input otherwise: the
// conversation read back from disk has no results, only what was asked for.

// diffCap bounds the lines one diff carries on the wire. The counts stay
// exact; the page says the rest was cut.
export const diffCap = 400;

const lines = (text) => (text === "" ? [] : String(text).replace(/\n$/, "").split("\n"));
const marked = (mark, text) => lines(text).map((l) => mark + l);

// patchHunks is Claude Code's structuredPatch as hunks, or null without one.
function patchHunks(result) {
  const patch = result && Array.isArray(result.structuredPatch) ? result.structuredPatch : [];
  if (!patch.length) return null;
  return patch.map((h) => ({ oldStart: h.oldStart || 0, newStart: h.newStart || 0, lines: (h.lines || []).map(String) }));
}

// Each builder turns one tool's input into hunks with no line numbers: all
// that is known is what went out and what came in.
const fromInput = {
  Edit: (input) => [{ oldStart: 0, newStart: 0, lines: [...marked("-", input.old_string || ""), ...marked("+", input.new_string || "")] }],
  Write: (input) => [{ oldStart: 0, newStart: 1, lines: marked("+", input.content || "") }],
  MultiEdit: (input) => (input.edits || []).map((e) => ({ oldStart: 0, newStart: 0, lines: [...marked("-", e.old_string || ""), ...marked("+", e.new_string || "")] })),
};

// toolDiff is the change one file-writing call made: its path, how many
// lines it added and removed, and the hunks (cut at diffCap lines). null for
// a tool that writes no file, or a call with no path yet (input still
// streaming).
export function toolDiff(tool, input, result) {
  const build = fromInput[tool];
  if (!build || !input || typeof input !== "object" || !input.file_path) return null;
  const hunks = patchHunks(result) || build(input);
  const all = hunks.flatMap((h) => h.lines);
  const count = (mark) => all.filter((l) => l[0] === mark).length;
  const diff = { path: String(input.file_path), added: count("+"), removed: count("-"), hunks: cut(hunks) };
  if (all.length > diffCap) diff.truncated = true;
  return diff;
}

// cut keeps the first diffCap lines across the hunks.
function cut(hunks) {
  const out = [];
  let room = diffCap;
  for (const h of hunks) {
    if (room <= 0) break;
    out.push({ ...h, lines: h.lines.slice(0, room) });
    room -= h.lines.length;
  }
  return out;
}

const statuses = ["pending", "in_progress", "completed"];
const statusOf = (s) => (statuses.includes(s) ? s : "pending");

// Each to-do tool's effect on the list. TodoWrite sends the whole list;
// the task tools change one item at a time, named by the id the create
// call's result gave (or its place in the list, read back from disk).
const todoTools = {
  TodoWrite: (prev, input) => (input.todos || []).map((t, i) => ({ id: String(i + 1), text: String(t.content || ""), status: statusOf(t.status) })),
  TaskCreate: (prev, input, result) => {
    const id = String((result && result.task && result.task.id) || prev.length + 1);
    return [...prev.filter((t) => t.id !== id), { id, text: String(input.subject || ""), status: "pending" }];
  },
  TaskUpdate: (prev, input) => {
    const id = String(input.taskId || "");
    if (input.status === "deleted") return prev.filter((t) => t.id !== id);
    return prev.map((t) => (t.id === id ? { ...t, text: input.subject ? String(input.subject) : t.text, status: input.status ? statusOf(input.status) : t.status } : t));
  },
};

// todosAfter is the to-do list after one finished tool call, or null when
// the tool is not a to-do tool (the list stands).
export function todosAfter(prev, tool, input, result) {
  const apply = todoTools[tool];
  if (!apply || !input || typeof input !== "object") return null;
  return apply(prev || [], input, result);
}
