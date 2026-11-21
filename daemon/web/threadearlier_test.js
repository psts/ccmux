// node --test threadearlier_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const m = require("./threadearlier.js");

const toBlocks = (turns) => turns.map((t) => ({ kind: "claude", id: t.id, text: t.text }));

test("each left conversation shows above, marked old and named apart, then a divider", () => {
  const now = new Date(2026, 10, 21, 15, 0).getTime();
  const left = [{ id: "c1", at: new Date(2026, 10, 21, 14, 5).getTime() }, { id: "c2", at: new Date(2026, 10, 21, 14, 30).getTime() }];
  const turns = { c1: [{ id: "a", text: "one" }], c2: [{ id: "a", text: "two" }] };
  const out = m.blocks(left, (c) => turns[c], toBlocks, now);
  assert.deepEqual(out.map((b) => b.kind + ":" + b.id), ["claude:old:c1:a", "divider:div:c1", "claude:old:c2:a", "divider:div:c2"]);
  assert.equal(out[0].old, true);
  assert.equal(out[1].text, "New clean conversation, started 14:05. Claude does not see what is above.");
  assert.deepEqual(m.blocks([], () => [], toBlocks, now), []);
  assert.deepEqual(m.blocks(undefined, () => [], toBlocks, now), []);
});

test("one not read yet is its divider alone, and only the newest few are drawn", () => {
  const now = new Date(2026, 10, 21, 15, 0).getTime();
  const left = Array.from({ length: m.SHOWN + 2 }, (_, i) => ({ id: "c" + i, at: now - 1000 * (10 - i) }));
  const out = m.blocks(left, () => undefined, toBlocks, now);
  assert.equal(out[0].text, "2 older conversations not shown.");
  assert.deepEqual(out.slice(1).map((b) => b.id), ["div:c2", "div:c3", "div:c4"]);
  assert.match(m.when(new Date(2026, 10, 19, 9, 7).getTime(), now), /^2026-11-19 09:07$/, "another day says the date");
});

test("load reads each shown left conversation once, and a seed is not read again", async () => {
  const asked = [];
  let redraws = 0;
  const e = m.create({
    call: async (method, path) => { asked.push(method + " " + path); if (path.endsWith("/bad")) throw new Error("gone"); return { turns: [{ id: "x" }] }; },
    snapshot: (frame) => ({ turns: frame.turns }),
    changed: () => redraws++,
  });
  e.seed("s1", "c2", [{ id: "seeded" }]);
  const sess = { id: "s1", leftConversations: [{ id: "c1", at: 1 }, { id: "c2", at: 2 }, { id: "bad", at: 3 }] };
  e.load(sess);
  e.load(sess);
  await new Promise((r) => setTimeout(r, 0));
  assert.deepEqual(asked, ["GET /v1/sessions/s1/conversations/c1", "GET /v1/sessions/s1/conversations/bad"]);
  const of = e.turnsFor("s1");
  assert.deepEqual([of("c1"), of("c2"), of("bad")], [[{ id: "x" }], [{ id: "seeded" }], { error: "gone" }]);
  assert.equal(redraws, 2, "a read and a failure each redraw");
  const out = m.blocks(sess.leftConversations, of, toBlocks, Date.now());
  assert.deepEqual(out.map((b) => b.id), ["old:c1:x", "div:c1", "old:c2:seeded", "div:c2", "unread:bad", "div:bad"]);
  assert.equal(out[4].text, "This earlier conversation could not be read (gone). Reload the page to try again.");
  assert.equal(out[4].level, "warning");
  e.load(null);
});
