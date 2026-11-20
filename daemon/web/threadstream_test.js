// node --test threadstream_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const st = require("./threadstream.js");

const ev = (seq, type, properties) => ({ t: "event", seq, data: { type, properties } });
const snapshot = (over = {}) => ({
  t: "snapshot", boot: "b1", seq: 10,
  data: {
    messages: [{ info: { id: "u1", role: "user" }, parts: [{ id: "u1.0", messageID: "u1", type: "text", text: "hi" }] }],
    permissions: [{ id: "abcde", permission: "Bash", patterns: ["ls"] }], questions: [], info: { model: "claude-haiku-5-5" }, result: null, todos: [], busy: true, sdk: "0.3.311", ...over,
  },
});

test("a snapshot is the whole conversation, and where its events start", () => {
  const s = st.apply(st.fresh(), snapshot(), 500).state;
  assert.deepEqual(st.cursor(s), { boot: "b1", after: 10 });
  assert.equal(s.turns[0].parts[0].text, "hi");
  assert.deepEqual(s.cards, [{ kind: "permission", id: "abcde", req: { id: "abcde", permission: "Bash", patterns: ["ls"] }, at: 500 }]);
  assert.deepEqual([s.busy, s.info.model, s.sdk], [true, "claude-haiku-5-5", "0.3.311"]);
});

test("events fold in once and in order: a reply streams into its part", () => {
  let s = st.apply(st.fresh(), snapshot(), 0).state;
  const steps = [
    ev(11, "message.updated", { info: { id: "m1", role: "assistant" } }),
    ev(12, "message.part.updated", { part: { id: "m1.0", messageID: "m1", type: "text", text: "" } }),
    ev(13, "message.part.delta", { messageID: "m1", partID: "m1.0", field: "text", delta: "Hel" }),
    ev(14, "message.part.delta", { messageID: "m1", partID: "m1.0", field: "text", delta: "lo" }),
  ];
  for (const f of steps) assert.equal(st.apply(s, f, 0).turn, "m1");
  assert.equal(s.turns[1].parts[0].text, "Hello");
  // The settled part replaces the streamed one, it is not added beside it.
  st.apply(s, ev(15, "message.part.updated", { part: { id: "m1.0", messageID: "m1", type: "text", text: "Hello." } }), 0);
  assert.deepEqual(s.turns[1].parts.map((p) => p.text), ["Hello."]);
  assert.equal(s.seq, 15);
});

test("an event seen already is skipped, and one that jumps ahead asks for a snapshot", () => {
  const s = st.apply(st.fresh(), snapshot(), 0).state;
  st.apply(s, ev(11, "message.updated", { info: { id: "m1", role: "assistant" } }), 0);
  const again = st.apply(s, ev(11, "message.updated", { info: { id: "m9", role: "assistant" } }), 0);
  assert.equal(again.skip, true);
  assert.equal(s.turns.length, 2, "the repeat drew nothing");
  const hole = st.apply(s, ev(14, "message.updated", { info: { id: "m3", role: "assistant" } }), 0);
  assert.equal(hole.resync, true);
  assert.deepEqual([s.seq, s.turns.length], [11, 2], "nothing after a hole is applied");
});

test("a new snapshot replaces the conversation: a restarted runner numbers from one again", () => {
  let s = st.apply(st.fresh(), snapshot(), 0).state;
  st.apply(s, { t: "state", state: "running" }, 0);
  s = st.apply(s, { t: "snapshot", boot: "b2", seq: 1, data: { messages: [], permissions: [], questions: [] } }, 0).state;
  assert.deepEqual(st.cursor(s), { boot: "b2", after: 1 });
  assert.deepEqual([s.turns.length, s.cards.length, s.where], [0, 0, "running"], "the cards of the old process are gone with it; where the session is stays said");
  assert.equal(st.apply(s, ev(2, "session.idle", {}), 0).meta, true);
});

test("a conversation read from disk has no process and no events to follow", () => {
  const s = st.apply(st.fresh(), { t: "snapshot", data: { messages: [{ info: { id: "u1", role: "user" }, parts: [] }] } }, 0).state;
  assert.deepEqual(st.cursor(s), { boot: "", after: 0 });
  assert.equal(s.turns.length, 1);
});

test("cards come and go by their id", () => {
  const s = st.apply(st.fresh(), snapshot(), 100).state;
  assert.equal(st.apply(s, ev(11, "question.asked", { id: "qqqqq", questions: [{ question: "Red or blue?" }] }), 700).cards, true);
  assert.deepEqual(s.cards.map((c) => [c.kind, c.id, c.at]), [["permission", "abcde", 100], ["question", "qqqqq", 700]]);
  st.apply(s, ev(12, "permission.replied", { requestID: "abcde", reply: "once" }), 0);
  st.apply(s, ev(13, "question.rejected", { requestID: "qqqqq" }), 0);
  st.apply(s, ev(14, "question.replied", { requestID: "never-there" }), 0);
  assert.deepEqual(s.cards, []);
});

test("a finished turn brings its bill, and what the session says of itself replaces what it said", () => {
  const s = st.apply(st.fresh(), snapshot(), 0).state;
  st.apply(s, ev(11, "session.info", { info: { model: "claude-opus-5-5", commands: [{ name: "cost" }] } }), 0);
  st.apply(s, ev(12, "session.todos", { todos: [{ id: "1", text: "Ship", status: "pending" }] }), 0);
  st.apply(s, ev(13, "session.error", { error: "overloaded" }), 0);
  st.apply(s, ev(14, "session.idle", { result: { costUSD: 0.02 } }), 0);
  assert.deepEqual([s.info.model, s.todos.length, s.error, s.busy, s.result.costUSD], ["claude-opus-5-5", 1, "overloaded", false, 0.02]);
  st.apply(s, ev(15, "session.idle", {}), 0);
  assert.equal(s.result.costUSD, 0.02, "an idle with no bill keeps the last one");
});

test("a failed turn's error is said until the next prompt, not through the turns after it", () => {
  const s = st.apply(st.fresh(), snapshot(), 0).state;
  st.apply(s, ev(11, "session.error", { error: "overloaded" }), 0);
  st.apply(s, ev(12, "session.idle", {}), 0);
  assert.equal(s.error, "overloaded", "still said once the turn has ended");
  // What does not end it: Claude Code's own lines, a note, a reply still arriving.
  st.apply(s, ev(13, "message.updated", { info: { id: "n1", role: "note" } }), 0);
  st.apply(s, ev(14, "message.updated", { info: { id: "h1", role: "user", synthetic: true } }), 0);
  st.apply(s, ev(15, "message.updated", { info: { id: "m7", role: "assistant" } }), 0);
  assert.equal(s.error, "overloaded");
  st.apply(s, ev(16, "message.updated", { info: { id: "u9", role: "user" } }), 0);
  assert.equal(s.error, "", "the human's next prompt is a new start");
  st.apply(s, ev(17, "session.idle", {}), 0);
  assert.equal(s.error, "");
});

test("a prompt or a reply means work is going; a note beside the conversation does not", () => {
  const s = st.apply(st.fresh(), snapshot({ busy: false }), 0).state;
  st.apply(s, ev(11, "message.updated", { info: { id: "n1", role: "note" } }), 0);
  assert.equal(s.busy, false);
  st.apply(s, ev(12, "message.updated", { info: { id: "u2", role: "user" } }), 0);
  assert.equal(s.busy, true);
});

test("an event of a kind this page does not know moves the number and draws nothing", () => {
  const s = st.apply(st.fresh(), snapshot(), 0).state;
  for (const [i, type] of ["session.created", "constructor", "__proto__", "toString"].entries()) {
    const out = st.apply(s, ev(11 + i, type, { x: 1 }), 0);
    assert.deepEqual(Object.keys(out), ["state"], type);
  }
  assert.equal(s.seq, 14);
  assert.deepEqual(Object.keys(st.apply(s, { t: "hello" }, 0)), ["state"]);
});

test("a delta for a part that is not there is dropped, not invented", () => {
  const s = st.apply(st.fresh(), snapshot(), 0).state;
  st.apply(s, ev(11, "message.part.delta", { messageID: "nope", partID: "nope.0", field: "text", delta: "x" }), 0);
  assert.equal(s.turns.length, 1);
});

test("a context event lands on the turn's result", () => {
  const s = st.apply(st.fresh(), snapshot(), 0).state;
  st.apply(s, ev(11, "session.idle", { result: { costUSD: 1 } }), 0);
  st.apply(s, ev(12, "session.context", { context: { percent: 42, used: 84000, max: 200000 } }), 0);
  assert.deepEqual(s.result, { costUSD: 1, context: { percent: 42, used: 84000, max: 200000 } });
  const t = st.apply(st.fresh(), snapshot(), 0).state;
  st.apply(t, ev(11, "session.context", { context: { percent: 5, used: 1, max: 2 } }), 0);
  assert.deepEqual(t.result, { context: { percent: 5, used: 1, max: 2 } });
});

test("the predicted next prompt is offered only between turns, with nothing typed and no card up, and a prompt spends it", () => {
  let s = st.fromSnapshot({ boot: "b", seq: 1, data: { messages: [], suggestion: "run the tests" } }, 0);
  assert.equal(st.offer(s, ""), "run the tests");
  assert.equal(st.offer(s, "  "), "run the tests", "blanks are nothing typed");
  assert.equal(st.offer(s, "fix"), "", "something typed wins");
  s = st.apply(s, { t: "event", seq: 2, data: { type: "session.suggestion", properties: { suggestion: "commit it" } } }, 0).state;
  assert.equal(st.offer(s, ""), "commit it");
  assert.equal(st.offer({ ...s, busy: true }, ""), "", "not while Claude works");
  assert.equal(st.offer({ ...s, cards: [{}] }, ""), "", "not while a card waits");
  s = st.apply(s, { t: "event", seq: 3, data: { type: "message.updated", properties: { info: { id: "u9", role: "user" } } } }, 0).state;
  assert.equal(s.suggestion, "", "the person's prompt spends it");
  assert.equal(st.offer(st.fresh(), ""), "");
});
