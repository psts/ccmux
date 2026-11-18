// settingsllm.js: what the Accounts tab's words and list changes mean.
// Run: node --test daemon/web/settingsllm_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const L = require("./settingsllm.js");

test("an account's health line names its state, why, and its usage", () => {
  assert.equal(L.statusLine(null), "");
  assert.equal(L.statusLine({ state: "ok", sessionPct: -1, weeklyPct: -1 }), "● active");
  assert.equal(L.statusLine({ state: "untried" }), "○ no traffic yet");
  assert.equal(L.statusLine({ state: "brand-new" }), "brand-new", "a state this lens has no words for shows as sent");
  assert.equal(L.statusLine({ state: "unreachable", lastError: "dial tcp: refused" }), "⚠ not answering (dial tcp: refused)");
  assert.equal(L.statusLine({ state: "ok", sessionPct: 40, weeklyPct: 7 }), "● active · session 40% · week 7%");
  const limited = L.statusLine({ state: "limited", limitedUntil: "2026-11-18T10:00:00Z" });
  assert.match(limited, /^◐ limited until \S/);
  assert.match(L.statusLine({ state: "ok", sessionPct: 0, sessionReset: "2026-11-18T10:00:00Z" }), /session 0% \(resets \S/);
});

test("only a meridian account has a sidecar line", () => {
  assert.equal(L.sidecarLine({ kind: "claude" }, { running: true, pid: 1 }), "");
  assert.equal(L.sidecarLine({ kind: "meridian" }, undefined), "⚙ sidecar not supervised by this daemon");
  assert.equal(L.sidecarLine({ kind: "meridian" }, { running: true, pid: 42 }), "⚙ sidecar running (pid 42)");
  assert.equal(L.sidecarLine({ kind: "meridian" }, { running: true, pid: 42, restarts: 3 }), "⚙ sidecar running (pid 42, 3 restarts)");
  assert.equal(L.sidecarLine({ kind: "meridian" }, { running: false, lastError: "exit 1" }), "⚙ sidecar stopped: exit 1");
  assert.equal(L.healthLine({ kind: "meridian" }, { state: "ok" }, { running: true, pid: 9 }), "● active · ⚙ sidecar running (pid 9)");
  assert.equal(L.healthLine({ kind: "openai" }, undefined, undefined), "");
});

test("the key box says what an empty key means for this account", () => {
  assert.equal(L.keyHintFor({ kind: "claude", apiKeySet: true }), "set — empty keeps it");
  assert.match(L.keyHintFor({ kind: "claude" }), /setup-token/);
  assert.match(L.keyHintFor({ kind: "meridian" }), /starts the sidecar/);
  assert.equal(L.keyHintFor({ kind: "openai" }), "empty = your own login");
});

test("Check is offered only where the daemon's own row says a claude account holds a token", () => {
  assert.equal(L.checkable(undefined), false);
  assert.equal(L.checkable({ kind: "claude", credentialSet: false }), false);
  assert.equal(L.checkable({ kind: "anthropic", credentialSet: true }), false);
  assert.equal(L.checkable({ kind: "claude", credentialSet: true }), true);
});

test("aliases round-trip through the text field, and half-typed ones are dropped", () => {
  const list = L.parseAliases("claude-haiku-*=qwen3-4b, a=b=c , broken, =x, y=");
  assert.deepEqual(list, [{ from: "claude-haiku-*", to: "qwen3-4b" }, { from: "a", to: "b=c" }]);
  assert.equal(L.aliasText(list), "claude-haiku-*=qwen3-4b, a=b=c");
  assert.equal(L.aliasText(undefined), "");
});

test("picking a model replaces the claude rules and keeps the custom ones", () => {
  assert.equal(L.aliasesWithModel("claude-haiku-*=old, gpt-*=keep, claude-*=older", "qwen3"), "gpt-*=keep, claude-*=qwen3");
  assert.equal(L.aliasesWithModel("", "qwen3"), "claude-*=qwen3");
  assert.equal(L.mappedModel({ modelAliases: [{ from: "gpt-*", to: "x" }, { from: "claude-*", to: "qwen3" }] }), "qwen3");
  assert.equal(L.mappedModel({}), "");
});

const names = (list) => list && list.map((a) => a.name);
const three = () => [{ name: "a" }, { name: "b" }, { name: "c" }];

test("a move is by name on the daemon's list, and a move off the end sends nothing", () => {
  assert.deepEqual(names(L.moved(three(), "b", -1)), ["b", "a", "c"]);
  assert.deepEqual(names(L.moved(three(), "b", 1)), ["a", "c", "b"]);
  assert.equal(L.moved(three(), "a", -1), null);
  assert.equal(L.moved(three(), "c", 1), null);
  assert.throws(() => L.moved(three(), "gone", 1), /"gone" is no longer in the list/);
  const before = three();
  L.moved(before, "b", 1);
  assert.deepEqual(names(before), ["a", "b", "c"], "the list handed in is not changed");
});

test("a remove is by name", () => {
  assert.deepEqual(names(L.removed(three(), "b")), ["a", "c"]);
  assert.deepEqual(names(L.removed(three(), "gone")), ["a", "b", "c"]);
});

test("an edit lands on the account it was opened on, a new one at the end", () => {
  const edited = L.upserted(three(), "b", { name: "b", kind: "openai" });
  assert.deepEqual(edited, [{ name: "a" }, { name: "b", kind: "openai" }, { name: "c" }]);
  assert.deepEqual(names(L.upserted(three(), "", { name: "d" })), ["a", "b", "c", "d"]);
});

test("an edit of an account deleted meanwhile is refused, never re-added without its key", () => {
  assert.throws(() => L.upserted(three(), "gone", { name: "gone" }), /was deleted while you had it open/);
});
