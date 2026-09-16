// node --test translate.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { Transcript, sessionInfo, questionRequest, questionAnswers, permissionPattern, questionCardText } from "./translate.mjs";

const now = () => 1000;

test("a streamed assistant turn becomes parts with deltas, then the complete message settles it", () => {
  const t = new Transcript("s1", now);
  assert.equal(t.userTurn("hi", "u1")[0].properties.info.role, "user");
  const ev = (event) => t.apply({ type: "stream_event", event });
  assert.equal(ev({ type: "message_start", message: { id: "m1" } })[0].type, "message.updated");
  const started = ev({ type: "content_block_start", index: 0, content_block: { type: "text", text: "" } });
  assert.equal(started[0].properties.part.id, "m1.0");
  const d = ev({ type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "hel" } });
  assert.deepEqual(d[0], { type: "message.part.delta", properties: { sessionID: "s1", messageID: "m1", partID: "m1.0", field: "text", delta: "hel" } });
  ev({ type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "lo" } });
  ev({ type: "content_block_start", index: 1, content_block: { type: "tool_use", id: "c1", name: "Bash", input: {} } });
  ev({ type: "content_block_delta", index: 1, delta: { type: "input_json_delta", partial_json: '{"command":' } });
  ev({ type: "content_block_delta", index: 1, delta: { type: "input_json_delta", partial_json: '"ls"}' } });
  const stopped = ev({ type: "content_block_stop", index: 1 });
  assert.equal(stopped[0].properties.part.state.title, "ls");
  assert.equal(stopped[0].properties.part.json, undefined, "the partial json buffer never reaches the wire");

  const settled = t.apply({ type: "assistant", message: { id: "m1", content: [{ type: "text", text: "hello" }, { type: "tool_use", id: "c1", name: "Bash", input: { command: "ls" } }] } });
  assert.equal(settled.length, 2);
  const msgs = t.messages();
  assert.equal(msgs.length, 2, "user + assistant, not a third turn for the complete message");
  assert.equal(msgs[1].parts[0].text, "hello");
  assert.equal(msgs[1].parts[1].state.input.command, "ls");

  const result = t.apply({ type: "user", message: { role: "user", content: [{ type: "tool_result", tool_use_id: "c1", content: "a.go" }] } });
  assert.equal(result[0].properties.part.state.status, "completed");
  assert.equal(result[0].properties.part.state.output, "a.go");
});

test("history from disk folds per-block assistant messages into one turn and keeps user prompts", () => {
  const t = new Transcript("s2", now);
  t.apply({ type: "user", uuid: "u1", message: { role: "user", content: "do it" } });
  t.apply({ type: "assistant", message: { id: "m1", content: [{ type: "thinking", thinking: "hm" }] } });
  t.apply({ type: "assistant", message: { id: "m1", content: [{ type: "tool_use", id: "c1", name: "Read", input: { file_path: "/x" } }] } });
  t.apply({ type: "user", uuid: "u2", message: { role: "user", content: [{ type: "tool_result", tool_use_id: "c1", content: [{ type: "text", text: "body" }], is_error: true }] } });
  t.apply({ type: "assistant", message: { id: "m1", content: [{ type: "text", text: "done" }] } });
  const msgs = t.messages();
  assert.equal(msgs.length, 2);
  assert.deepEqual(msgs[1].parts.map((p) => p.type), ["reasoning", "tool", "text"]);
  assert.equal(msgs[1].parts[1].state.status, "error");
  assert.equal(msgs[1].parts[1].state.error, "body");
  assert.equal(msgs[0].info.id, "u1");
});

test("an interrupted or failed turn carries its error on the assistant turn", () => {
  const t = new Transcript("s3", now);
  t.apply({ type: "assistant", message: { id: "m1", content: [{ type: "text", text: "1\n2" }] } });
  const evs = t.finishTurn("rate limited");
  assert.equal(evs[0].properties.info.error.data.message, "rate limited");
  assert.equal(t.finishTurn("again").length, 0, "one error per turn");
  assert.equal(new Transcript("s4", now).finishTurn("boot").length, 0, "nothing to land on: no turn is invented here");
});

test("a failure with no turn to land on gets an assistant turn of its own", () => {
  const boot = new Transcript("s5", now);
  const [ev] = boot.errorTurn("exited with code 1");
  assert.equal(ev.type, "message.updated");
  assert.equal(ev.properties.info.role, "assistant");
  assert.equal(boot.messages()[0].info.error.data.message, "exited with code 1");
  assert.deepEqual(boot.messages()[0].parts, []);
});

test("sessions, questions and permission patterns map onto the chat's shapes", () => {
  assert.deepEqual(sessionInfo({ sessionId: "a", summary: "Sum", cwd: "/d", lastModified: 5, createdAt: 1 }), { id: "a", title: "Sum", directory: "/d", time: { created: 1, updated: 5 } });
  assert.equal(sessionInfo({ sessionId: "b", firstPrompt: "first line\nsecond" }).title, "first line");
  const input = { questions: [{ question: "Tea or coffee?", header: "Drink", options: [{ label: "Tea", description: "calm" }], multiSelect: false }, { question: "Which?", options: [{ label: "A" }, { label: "B" }], multiSelect: true }] };
  const req = questionRequest("que_1", "s", input, "c9");
  assert.equal(req.questions[0].header, "Drink");
  assert.equal(req.questions[1].multiple, true);
  assert.deepEqual(questionAnswers(input, [["Tea"], ["A", "B"]]), { "Tea or coffee?": "Tea", "Which?": ["A", "B"] });
  assert.deepEqual(questionAnswers(input, [[], ["B"]]), { "Which?": ["B"] });
  assert.equal(permissionPattern("Bash", { command: "git status" }), "git status");
  assert.equal(permissionPattern("Odd", { a: 1 }), '{"a":1}');
});

test("a question card renders for the bus as lines a peer can answer in words", () => {
  const req = questionRequest("abcde", "s1", { questions: [
    { question: "Which tone?", header: "Tone", options: [{ label: "Warm", description: "friendly" }, { label: "Dry" }] },
    { question: "Post now?", options: [{ label: "Yes" }], multiSelect: true },
  ] }, "c1");
  assert.equal(questionCardText(req), "Tone: Which tone?\n  - Warm: friendly\n  - Dry\nPost now? (one or more)\n  - Yes");
  assert.equal(questionCardText({ questions: [] }), "");
});
