// node --test threadedit_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const E = require("./threadedit.js");

test("the question says what leaves, what happens to the files, and what cannot be undone", () => {
  const one = E.question({ dropped: 1, files: { canRewind: true, files: ["/r/app/a.txt"] } }, { files: [] });
  assert.match(one, /This message and Claude's answer to it leave the conversation/);
  assert.match(one, /put back \(1\): a\.txt\./);
  assert.match(one, /Commands Claude ran .* are not undone/);
  assert.doesNotMatch(one, /pictures/);
  const many = E.question({ dropped: 3, files: { canRewind: true, files: ["a", "b", "c", "d", "e", "f", "g"] } }, { files: [{ name: "x.png" }] });
  assert.match(many, /the 2 after it/);
  assert.match(many, /\(7\): a, b, c, d, e and 2 more\./);
  assert.match(many, /pictures are not put back/);
  assert.match(E.question({ dropped: 1, files: { canRewind: true, files: [] } }, {}), /No files were changed since/);
  assert.match(E.question({ dropped: 1, files: { canRewind: false, error: "No file checkpoint found for this message." } }, {}), /cannot be put back \(No file checkpoint found for this message\): only the conversation goes back/);
});

function rig(over = {}) {
  const calls = [], asked = [], filled = [], said = [], failed = [];
  let tries = 0;
  const d = {
    busy: () => !!over.busy,
    confirm: (q) => { asked.push(q); return over.answers ? over.answers.shift() : true; },
    wait: async () => {},
    fill: (t) => filled.push(t), done: (t) => said.push(t), fail: (t) => failed.push(t),
    call: async (method, url, body) => {
      calls.push([method, url, body]);
      if (url.endsWith("/rewind") && body.dry && tries++ < (over.workingFor || 0)) throw new Error("Claude is working; stop the turn first");
      if (over.refuse && url.endsWith("/rewind")) throw new Error(over.refuse);
      if (url.endsWith("/rewind")) return body.dry ? { dropped: 1, files: { canRewind: true, files: [] } } : { dropped: 1, files: { canRewind: true }, conversation: "f1" };
      return {};
    },
  };
  return { e: E.create(d), calls, asked, filled, said, failed };
}

test("edit looks, asks, goes back and puts the words back in the box", async () => {
  const r = rig();
  await r.e.edit("s1", { id: "p2", text: "fix the build", files: [] });
  assert.deepEqual(r.calls, [["POST", "/v1/sessions/s1/rewind", { to: "p2", dry: true }], ["POST", "/v1/sessions/s1/rewind", { to: "p2" }]]);
  assert.equal(r.asked.length, 1);
  assert.deepEqual(r.filled, ["fix the build"]);
  assert.match(r.said[0], /Went back/);
});

test("a no leaves everything as it was", async () => {
  const r = rig({ answers: [false] });
  await r.e.edit("s1", { id: "p2", text: "x" });
  assert.equal(r.calls.length, 1, "only the look");
  assert.deepEqual(r.filled, []);
});

test("while Claude works it is stopped first, after a yes, and the look waits for the turn to end", async () => {
  const r = rig({ busy: true, workingFor: 2 });
  await r.e.edit("s1", { id: "p2", text: "x" });
  assert.deepEqual(r.calls.map((c) => c[1] + (c[2] && c[2].dry ? " dry" : "")), [
    "/v1/sessions/s1/abort", "/v1/sessions/s1/rewind dry", "/v1/sessions/s1/rewind dry", "/v1/sessions/s1/rewind dry", "/v1/sessions/s1/rewind",
  ]);
  assert.match(r.asked[0], /Stop it and go back/);
  const no = rig({ busy: true, answers: [false] });
  await no.e.edit("s1", { id: "p2", text: "x" });
  assert.deepEqual(no.calls, [], "not stopped without a yes");
});

test("a refusal is said, and the box is left alone", async () => {
  const r = rig({ refuse: "that message is not in this conversation" });
  await r.e.edit("s1", { id: "p2", text: "x" });
  assert.deepEqual(r.failed, ["Going back failed: that message is not in this conversation"]);
  assert.deepEqual(r.filled, []);
});
