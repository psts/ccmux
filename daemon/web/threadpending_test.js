// node --test threadpending_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const P = require("./threadpending.js");

const turn = (id, role = "user") => ({ info: { id, role }, parts: [] });
const msg = (key, where, state = "new", over = {}) => ({ key, where, state, id: "", error: "", text: "m" + key, urls: [], ...over });

test("a pending message leaves once the stream holds its user turn, by the prompt's id", () => {
  const list = [msg(1, "a", "queued", { id: "p1" }), msg(2, "a"), msg(3, "a", "queued", { id: "p3" })];
  const out = P.arrived(list, [turn("p1"), turn("p3", "assistant")]);
  assert.deepEqual(out.landed.map((p) => p.key), [1]);
  assert.deepEqual(out.left.map((p) => p.key), [2, 3], "no id yet, or only an assistant turn of that id: still pending");
});

test("the next to send is the oldest new one, and a failed one holds only its own conversation", () => {
  const list = [msg(1, "a", "queued"), msg(2, "a", "failed"), msg(3, "a"), msg(4, "b")];
  assert.equal(P.next(list).key, 4, "3 waits behind the failed 2; b is not held by a");
  assert.equal(P.held(list, list[2]), true);
  assert.equal(P.held(list, list[3]), false);
  list[1].state = "new"; // Retry
  assert.equal(P.next(list).key, 2, "the retried one goes first, before the one that waited");
  assert.equal(P.next([msg(1, "a", "queued")]), null);
});

test("only the ones sent where the page is are drawn, each saying what it is doing", () => {
  const list = [msg(1, "a", "failed", { error: "HTTP 500", urls: ["blob:1"] }), msg(2, "a"), msg(3, "b"), msg(4, "a", "queued", { id: "p4" })];
  const b = P.blocks(list, "a");
  assert.deepEqual(b.map((x) => [x.id, x.pending, x.error]), [["pending:1", "failed", "HTTP 500"], ["pending:2", "held", ""], ["pending:4", "queued", ""]]);
  assert.deepEqual(b[0], { kind: "user", id: "pending:1", key: 1, text: "m1", files: [], urls: ["blob:1"], pending: "failed", error: "HTTP 500" });
  assert.equal(P.blocks(list, "b")[0].pending, "sending");
});

test("a draft's messages follow it to the session it became", () => {
  const list = [msg(1, "draft"), msg(2, "other")];
  P.rehome(list, "draft", "sess", { kind: "session", id: "s1" });
  assert.deepEqual(list.map((p) => p.where), ["sess", "other"]);
  assert.deepEqual(list[0].sel, { kind: "session", id: "s1" }, "and is sent there");
  assert.equal(list[1].sel, undefined);
});
