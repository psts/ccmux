// node --test threadmention_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const M = require("./threadmention.js");

test("a mention is the @word where the cursor is, never an @ inside a word", () => {
  assert.deepEqual(M.mentionAt("look at @src/he", 15), { start: 8, query: "src/he" });
  assert.deepEqual(M.mentionAt("@", 1), { start: 0, query: "" });
  assert.deepEqual(M.mentionAt("fix @a.go and more", 9), { start: 4, query: "a.go" });
  assert.equal(M.mentionAt("mail me@host.se", 15), null, "an email is not a mention");
  assert.equal(M.mentionAt("@a.go done", 10), null, "the cursor is past it");
  assert.equal(M.mentionAt("no at here", 10), null);
});

test("a pick puts the path in place of what was typed, with one space after", () => {
  const at = M.mentionAt("look at @he please", 11);
  assert.deepEqual(M.pick("look at @he please", 11, at, "src/head.go"), { text: "look at @src/head.go please", caret: 21 });
  const end = M.mentionAt("@he", 3);
  assert.deepEqual(M.pick("@he", 3, end, "src/head.go"), { text: "@src/head.go ", caret: 13 });
});
