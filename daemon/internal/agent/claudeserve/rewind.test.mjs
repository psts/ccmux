// node --test rewind.test.mjs — going back to an earlier prompt, against a
// stub agent: where the conversation is cut, what a look says, and what
// going back does to the files, the conversation and later prompts.
import { test } from "node:test";
import assert from "node:assert/strict";
import { isPrompt, forkPoint, rewind } from "./rewind.mjs";

const user = (uuid, content) => ({ type: "user", uuid, message: { role: "user", content } });
const reply = (uuid) => ({ type: "assistant", uuid, message: { role: "assistant", content: [{ type: "text", text: "ok" }] } });
const chain = [
  user("p1", "make a.txt"), reply("r1"),
  user("t1", [{ type: "tool_result", tool_use_id: "x", content: "done" }]), reply("r2"),
  user("p2", [{ type: "text", text: "now b.txt" }]), reply("r3"),
  user("p3", "and c.txt"), reply("r4"),
  user("x1", [{ type: "text", text: "[Request interrupted by user]" }]),
];

test("a prompt is a person's words or picture, never a tool's result", () => {
  assert.equal(isPrompt(chain[0]), true);
  assert.equal(isPrompt(chain[4]), true);
  assert.equal(isPrompt(user("i", [{ type: "image", source: {} }])), true);
  assert.equal(isPrompt(chain[2]), false);
  assert.equal(isPrompt(chain[1]), false);
  assert.equal(isPrompt(user("e", "  ")), false);
  assert.equal(isPrompt(user("s", [{ type: "text", text: "[Request interrupted by user]" }])), false, "Claude Code's line about a stopped turn");
  assert.equal(isPrompt(user("s", "[Request interrupted by user for tool use]")), false);
  assert.equal(isPrompt({ ...user("m", "caveat"), isMeta: true }), false);
});

test("the cut is just before the prompt, and every prompt from it on goes", () => {
  assert.deepEqual(forkPoint(chain, "p2"), { after: "r2", dropped: 2 });
  assert.deepEqual(forkPoint(chain, "p3"), { after: "r3", dropped: 1 });
  assert.deepEqual(forkPoint(chain, "p1"), { after: null, dropped: 3 }, "the first prompt keeps nothing");
  assert.match(forkPoint(chain, "t1").error, /not a prompt/);
  assert.match(forkPoint(chain, "gone").error, /not in this conversation/);
});

function stubAgent(over = {}) {
  const calls = [];
  const a = {
    busy: false, rewound: false, sid: "s1", cwd: "/w", q: null,
    ensureQuery() { calls.push("ensure"); a.q = a.q || q; },
    sdk: {
      getSessionMessages: async (id, o) => { calls.push(["read", id, o.dir]); return chain; },
      forkSession: async (id, o) => { calls.push(["fork", id, o]); return { sessionId: "f1" }; },
    },
    ...over,
  };
  const q = {
    rewindFiles: async (to, o) => {
      calls.push(["files", to, !!(o && o.dryRun)]);
      if (over.filesThrow) throw new Error("No file checkpoint found for this message.");
      return o && o.dryRun ? { canRewind: true, filesChanged: ["/w/b.txt"] } : { canRewind: over.realFails ? false : true, error: over.realFails ? "backup missing" : undefined };
    },
  };
  return { a, calls };
}

test("a look says what goes and touches nothing", async () => {
  const { a, calls } = stubAgent();
  assert.deepEqual(await rewind(a, "p2", true), { dropped: 2, files: { canRewind: true, error: "", files: ["/w/b.txt"] } });
  assert.deepEqual(calls, [["read", "s1", "/w"], "ensure", ["files", "p2", true]]);
  assert.equal(a.rewound, false);
});

test("going back puts the files back, forks before the prompt, and takes no more prompts", async () => {
  const { a, calls } = stubAgent();
  const out = await rewind(a, "p2", false);
  assert.equal(out.conversation, "f1");
  assert.deepEqual(calls.slice(-2), [["files", "p2", false], ["fork", "s1", { dir: "/w", upToMessageId: "r2" }]]);
  assert.equal(a.rewound, true);
  await assert.rejects(rewind(a, "p3", true), /already going back/);
});

test("going back to the first prompt opens a new conversation, with no fork", async () => {
  const { a, calls } = stubAgent();
  const out = await rewind(a, "p1", false);
  assert.match(out.conversation, /^[0-9a-f-]{36}$/);
  assert.ok(!calls.some((c) => c[0] === "fork"));
});

test("files that cannot be put back are said, and the conversation still goes back", async () => {
  const { a, calls } = stubAgent({ filesThrow: true });
  const look = await rewind(a, "p2", true);
  assert.deepEqual(look.files, { canRewind: false, error: "No file checkpoint found for this message.", files: [] });
  const out = await rewind(a, "p2", false);
  assert.equal(out.conversation, "f1");
  assert.equal(calls.filter((c) => c[0] === "files" && !c[2]).length, 0, "no real undo is tried");
  const late = stubAgent({ realFails: true });
  const o2 = await rewind(late.a, "p2", false);
  assert.deepEqual([o2.files.canRewind, o2.files.error], [false, "backup missing"]);
});

test("a busy session or a failed fork goes back nowhere", async () => {
  await assert.rejects(rewind(stubAgent({ busy: true }).a, "p2", true), /stop the turn first/);
  const { a } = stubAgent();
  a.sdk.forkSession = async () => { throw new Error("disk full"); };
  await assert.rejects(rewind(a, "p2", false), /files were put back, but the conversation could not be cut: disk full/);
  assert.equal(a.rewound, false, "it takes prompts again");
});
