// node --test richtranscript.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { RichTranscript, promptView, noteFor } from "./richtranscript.mjs";
import { Transcript } from "./translate.mjs";
import { MediaStore, mediaName } from "./media.mjs";

const now = () => 1000;
function store() {
  const files = new Map();
  return new MediaStore("/m", { mkdir() {}, exists: (p) => files.has(p), write: (p, b) => files.set(p, b), read: (p) => files.get(p) });
}
const png = Buffer.from("\x89PNG bytes", "binary").toString("base64");
const image = { type: "image", source: { type: "base64", media_type: "image/png", data: png } };
const toolUse = (id, name, input) => ({ type: "assistant", message: { id: "m-" + id, content: [{ type: "tool_use", id, name, input }] } });
const toolResult = (id, content, extra = {}) => ({ type: "user", message: { role: "user", content: [{ type: "tool_result", tool_use_id: id, content, ...extra.block }] }, tool_use_result: extra.result });
const lastPart = (events) => events.filter((e) => e.type === "message.part.updated").pop().properties.part;

test("an edit shows what it will change before it runs, and Claude Code's own patch once it has", () => {
  const t = new RichTranscript("s", now);
  const asked = lastPart(t.apply(toolUse("c1", "Edit", { file_path: "/r/a.txt", old_string: "world", new_string: "there" })));
  assert.deepEqual(asked.state.diff.hunks, [{ oldStart: 0, newStart: 0, lines: ["-world", "+there"] }]);
  const done = lastPart(t.apply(toolResult("c1", "updated", { result: { structuredPatch: [{ oldStart: 1, newStart: 1, lines: [" hello", "-world", "+there"] }] } })));
  assert.equal(done.state.status, "completed");
  assert.deepEqual(done.state.diff.hunks, [{ oldStart: 1, newStart: 1, lines: [" hello", "-world", "+there"] }]);
  assert.deepEqual([done.state.diff.added, done.state.diff.removed], [1, 1]);
});

test("a conversation read back from disk has no results, and still shows its diffs", () => {
  const t = new RichTranscript("s", now);
  t.apply(toolUse("c1", "Write", { file_path: "/r/n.txt", content: "one\ntwo\n" }));
  const done = lastPart(t.apply(toolResult("c1", "File created")));
  assert.deepEqual(done.state.diff, { path: "/r/n.txt", added: 2, removed: 0, hunks: [{ oldStart: 0, newStart: 1, lines: ["+one", "+two"] }] });
});

test("a write that failed changed nothing, so it shows no diff", () => {
  const t = new RichTranscript("s", now);
  t.apply(toolUse("c1", "Edit", { file_path: "/r/a.txt", old_string: "x", new_string: "y" }));
  const failed = lastPart(t.apply(toolResult("c1", "String to replace not found", { block: { is_error: true } })));
  assert.equal(failed.state.status, "error");
  assert.equal(failed.state.diff, undefined);
});

test("a streamed tool call gets its diff when its input is whole", () => {
  const t = new RichTranscript("s", now);
  const ev = (event) => t.apply({ type: "stream_event", event });
  ev({ type: "message_start", message: { id: "m1" } });
  ev({ type: "content_block_start", index: 0, content_block: { type: "tool_use", id: "c1", name: "Write", input: {} } });
  ev({ type: "content_block_delta", index: 0, delta: { type: "input_json_delta", partial_json: '{"file_path":"/r/x","content":"a\\nb"}' } });
  assert.equal(lastPart(ev({ type: "content_block_stop", index: 0 })).state.diff.added, 2);
});

test("images a tool returned are kept as files and named on its part", () => {
  const media = store();
  const t = new RichTranscript("s", now, media);
  t.apply(toolUse("c1", "Read", { file_path: "/r/shot.png" }));
  const done = lastPart(t.apply(toolResult("c1", [image, { type: "text", text: "a screenshot" }])));
  assert.equal(done.state.images.length, 1);
  assert.match(done.state.images[0].name, mediaName);
  assert.equal(done.state.output, "a screenshot");
  assert.ok(!JSON.stringify(done).includes(png), "the bytes are not on the wire");
});

test("a prompt's images become file parts after its text", () => {
  const t = new RichTranscript("s", now, store());
  const events = t.apply({ type: "user", uuid: "u1", message: { role: "user", content: [image, { type: "text", text: "what is this?" }] } });
  const parts = t.messages()[0].parts;
  assert.deepEqual(parts.map((p) => p.type), ["text", "file"]);
  assert.equal(parts[0].text, "what is this?");
  assert.deepEqual(Object.keys(parts[1]), ["id", "messageID", "sessionID", "type", "mime", "name"]);
  assert.equal(parts[1].id, "u1.1");
  assert.equal(events.length, 3);
  // A picture with no words is still a prompt.
  t.apply({ type: "user", uuid: "u2", message: { role: "user", content: [image] } });
  assert.deepEqual(t.messages()[1].parts.map((p) => p.type), ["text", "file"]);
});

test("with no store the images are left out, as the chat has them", () => {
  const t = new RichTranscript("s", now);
  t.apply({ type: "user", uuid: "u1", message: { role: "user", content: [image, { type: "text", text: "hi" }] } });
  assert.deepEqual(t.messages()[0].parts.map((p) => p.type), ["text"]);
  assert.equal(t.apply({ type: "user", uuid: "u2", message: { role: "user", content: [image] } }).length, 0);
});

test("a to-do tool moves the session's list and says so once", () => {
  const t = new RichTranscript("s", now);
  t.apply(toolUse("c1", "TodoWrite", { todos: [{ content: "Write the test", status: "in_progress" }] }));
  const events = t.apply(toolResult("c1", "Todos have been modified"));
  const said = events.filter((e) => e.type === "session.todos");
  assert.deepEqual(said, [{ type: "session.todos", properties: { sessionID: "s", todos: [{ id: "1", text: "Write the test", status: "in_progress" }] } }]);
  assert.deepEqual(t.todos, said[0].properties.todos);
  t.apply(toolUse("c2", "Bash", { command: "ls" }));
  assert.equal(t.apply(toolResult("c2", "a.go")).filter((e) => e.type === "session.todos").length, 0, "another tool says nothing of the list");
});

test("a slash command reads as it was typed, its output as a note, and model-only lines not at all", () => {
  assert.deepEqual(promptView("<command-message>hello</command-message>\n<command-name>/hello</command-name>\n<command-args>world42</command-args>"), { text: "/hello world42" });
  assert.deepEqual(promptView("<command-name>/context</command-name>\n<command-message>context</command-message>\n<command-args></command-args>"), { text: "/context" });
  assert.deepEqual(promptView("<local-command-stdout>42 tokens</local-command-stdout>"), { text: "42 tokens", mark: { synthetic: true } });
  assert.deepEqual(promptView("<local-command-caveat>Caveat: the messages below…</local-command-caveat>"), { text: null });
  const notice = "<task-notification>\n<task-id>bmjrt2r7n</task-id>\n<status>completed</status>\n<summary>Background command \"docker run --rm -p 8090:80 site\" completed (exit code 0)</summary>\n</task-notification>";
  assert.deepEqual(promptView(notice), { text: "Background command \"docker run --rm -p 8090:80 site\" completed (exit code 0)", mark: { synthetic: true } },
    "a background task's end is Claude Code's note, not the person's words");
  assert.equal(promptView("<task-notification><summary>Background command \"make &gt;/dev/null &amp;&amp; run &amp;lt;x\" completed</summary></task-notification>").text,
    "Background command \"make >/dev/null && run &lt;x\" completed", "Claude Code's escapes are read back once");
  assert.deepEqual(promptView("anything", { origin: { kind: "task-notification" } }), { text: "A background task ended.", mark: { synthetic: true } }, "marked on disk, whatever the text");
  assert.deepEqual(promptView("<task-notification> typed about by a person"), { text: "A background task ended.", mark: { synthetic: true } });
  assert.deepEqual(promptView("fix the bug"), { text: "fix the bug" });
  assert.deepEqual(promptView("Stop hook feedback:\nrun the gate", { isSynthetic: true }), { text: "Stop hook feedback:\nrun the gate", mark: { synthetic: true } });
});

test("a message Claude Code wrote itself is marked, and a hidden one makes no turn", () => {
  const t = new RichTranscript("s", now);
  t.apply({ type: "user", uuid: "u1", isSynthetic: true, message: { role: "user", content: [{ type: "text", text: "Stop hook feedback: run the gate" }] } });
  assert.equal(t.messages()[0].info.synthetic, true);
  t.apply({ type: "user", uuid: "u2", message: { role: "user", content: "typed by a human" } });
  assert.equal(t.messages()[1].info.synthetic, undefined);
  assert.equal(t.apply({ type: "user", uuid: "u3", message: { role: "user", content: "<local-command-caveat>x</local-command-caveat>" } }).length, 0);
  assert.equal(t.messages().length, 2);
});

test("what Claude Code says beside the conversation is a note where it happened", () => {
  const t = new RichTranscript("s", now);
  t.userTurn("hi", "u1");
  const events = t.note(noteFor({ type: "system", subtype: "informational", content: "Stop says: Gate: 1 source file(s) unverified (block 1/3)", level: "notice" }), "notice");
  assert.equal(events[0].properties.info.role, "note");
  assert.equal(events[0].properties.info.level, "notice");
  assert.equal(t.messages()[1].parts[0].text, "Stop says: Gate: 1 source file(s) unverified (block 1/3)");
  assert.match(noteFor({ type: "system", subtype: "api_retry", attempt: 2, max_retries: 10, error_status: 529 }), /attempt 2 of 10/);
  assert.equal(noteFor({ type: "system", subtype: "thinking_tokens" }), "");
  assert.deepEqual(t.note(""), [], "nothing to say, no turn");
});

test("the agent chat's transcript has none of it", () => {
  const t = new Transcript("s", now);
  t.apply(toolUse("c1", "Edit", { file_path: "/r/a.txt", old_string: "a", new_string: "b" }));
  const events = t.apply(toolResult("c1", [image, { type: "text", text: "ok" }], { result: { structuredPatch: [{ lines: ["-a", "+b"] }] } }));
  assert.deepEqual(Object.keys(lastPart(events).state), ["status", "input", "output", "title"]);
  assert.equal(events.length, 1);
  t.apply({ type: "user", uuid: "u1", isSynthetic: true, message: { role: "user", content: [image, { type: "text", text: "hi" }] } });
  assert.deepEqual(t.messages().pop(), { info: { id: "u1", sessionID: "s", role: "user", time: { created: 1000 } }, parts: [{ id: "u1.0", messageID: "u1", sessionID: "s", type: "text", text: "hi" }] });
});

test("a note mid-turn does not take the reply's place: a failure after it still lands on the reply", () => {
  const t = new RichTranscript("s", now);
  t.userTurn("go", "u1");
  t.apply(toolUse("c1", "Bash", { command: "make" }));
  t.apply(toolResult("c1", "ok"));
  t.note(noteFor({ type: "system", subtype: "api_retry", attempt: 1, max_retries: 2, error_status: 529 }));
  const events = t.finishTurn("overloaded");
  assert.equal(events.length, 1);
  assert.equal(events[0].properties.info.id, "m-c1");
  assert.deepEqual(t.messages().find((m) => m.info.id === "m-c1").info.error, { name: "error", data: { message: "overloaded" } });
  assert.equal(t.messages().pop().info.error, undefined, "the note itself carries no error");
});

test("a note after the human's prompt still means no reply is being written", () => {
  const t = new RichTranscript("s", now);
  t.apply({ type: "assistant", message: { id: "m1", content: [{ type: "text", text: "done" }] } });
  t.userTurn("next", "u2");
  t.note("Stop says: no");
  assert.equal(t.currentAssistant(), null);
  assert.deepEqual(t.finishTurn("died before replying"), [], "the finished reply m1 is not marked as the one that failed");
  assert.equal(t.messages()[0].info.error, undefined);
});

test("a reply goes on streaming after a note arrived in the middle of it", () => {
  const t = new RichTranscript("s", now);
  const ev = (event) => t.apply({ type: "stream_event", event });
  ev({ type: "message_start", message: { id: "m1" } });
  t.note("The API did not answer. Trying again.");
  const started = ev({ type: "content_block_start", index: 0, content_block: { type: "text", text: "" } });
  assert.equal(started.length, 1, "the block is not dropped");
  ev({ type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "still here" } });
  assert.equal(t.messages().find((m) => m.info.id === "m1").parts[0].text, "still here");
});

test("what a helper agent says and is told stays out; the call and its result stay in", () => {
  const t = new RichTranscript("s", now);
  t.apply(toolUse("a1", "Agent", { description: "Fetch the CVE", prompt: "Fetch https://example.invalid and report" }));
  const helper = [
    { type: "user", parent_tool_use_id: "a1", message: { role: "user", content: [{ type: "text", text: "Fetch https://example.invalid and report" }] } },
    { type: "stream_event", parent_tool_use_id: "a1", event: { type: "message_start", message: { id: "h1" } } },
    { type: "assistant", parent_tool_use_id: "a1", message: { id: "h1", content: [{ type: "text", text: "The record is reserved." }] } },
  ];
  for (const m of helper) assert.deepEqual(t.apply(m), [], "nothing from the helper");
  t.apply(toolResult("a1", "The record is reserved."));
  const turns = t.messages();
  assert.deepEqual(turns.map((x) => x.info.role), ["assistant"], "no user turn, no turn of the helper's own");
  const call = turns[0].parts.find((p) => p.callID === "a1");
  assert.equal(call.state.status, "completed", "the call and its result are the conversation's");
});
