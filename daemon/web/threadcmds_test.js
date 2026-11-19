// node --test threadcmds_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const C = require("./threadcmds.js");

const cmds = (...names) => names.map((name) => ({ name, description: "", hint: "" }));
const names = (info) => (info.commands || []).map((c) => c.name).join(",");

test("a draft shows the list its own repo and kind said last", () => {
  let m = C.remember({}, "w1", true, { commands: cmds("review"), terminalCommands: ["doctor"] }, 1);
  m = C.remember(m, "w1", false, { commands: cmds("cost") }, 2);
  assert.equal(names(C.forDraft(m, "w1", true)), "review");
  assert.deepEqual(C.forDraft(m, "w1", true).terminalCommands, ["doctor"]);
  assert.equal(names(C.forDraft(m, "w1", false)), "cost");
});

test("a repo with no list borrows its other kind's, then the newest of any repo, then nothing", () => {
  let m = C.remember({}, "w1", true, { commands: cmds("old") }, 1);
  m = C.remember(m, "w2", false, { commands: cmds("new") }, 2);
  assert.equal(names(C.forDraft(m, "w2", true)), "new", "same repo, other kind");
  assert.equal(names(C.forDraft(m, "w3", false)), "new", "newest of any");
  assert.deepEqual(C.forDraft({}, "w1", true), {});
  assert.deepEqual(C.forDraft(undefined, "w1", true), {});
});

test("info without commands changes nothing, and terminal commands outlive a list that lacks them", () => {
  const m = C.remember({}, "w1", false, { commands: cmds("a"), terminalCommands: ["doctor"] }, 1);
  assert.equal(C.remember(m, "w1", false, { model: "x" }, 2), m);
  assert.equal(C.remember(m, "", false, { commands: cmds("b") }, 2), m);
  const later = C.remember(m, "w1", false, { commands: cmds("a", "b") }, 3);
  assert.deepEqual(later["w1:thread"].terminalCommands, ["doctor"]);
  assert.equal(C.remember(later, "w1", false, { commands: cmds("a", "b") }, 4)["w1:thread"].at, 4, "seen again counts as newest");
  assert.equal(m["w1:thread"].commands.length, 1, "the input is not changed");
});

test("only the newest thirty lists are kept", () => {
  let m = {};
  for (let i = 0; i < 35; i++) m = C.remember(m, "w" + i, false, { commands: cmds("c" + i) }, i);
  assert.equal(Object.keys(m).length, 30);
  assert.equal(m["w0:thread"], undefined);
  assert.ok(m["w34:thread"]);
});

test("load and save survive blocked or broken storage", () => {
  const blocked = { getItem() { throw new Error("blocked"); }, setItem() { throw new Error("blocked"); } };
  assert.deepEqual(C.load(blocked), {});
  C.save(blocked, { a: 1 });
  assert.deepEqual(C.load({ getItem: () => "{oops" }), {});
  assert.deepEqual(C.load({ getItem: () => "[1]" }), {});
  const kept = {};
  const mem = { getItem: (k) => kept[k] || null, setItem: (k, v) => { kept[k] = v; } };
  const m = C.remember({}, "w1", true, { commands: cmds("x") }, 1);
  C.save(mem, m);
  assert.deepEqual(C.load(mem), m);
});
