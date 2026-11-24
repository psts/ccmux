// node --test sessionself.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { advisorOf, settingsEffort, backgroundTasks, commandList, contextOf, fromInit, fromInitialization, resultOf } from "./sessionself.mjs";

test("commands are listed once, the first row of a name standing", () => {
  const list = commandList([
    { name: "cost", description: "What this session cost", argumentHint: "", builtin: true },
    { name: "hello", description: "Say hello (project)", argumentHint: "<name>" },
    { name: "cost", description: "A plugin's cost" },
    { description: "no name" },
  ]);
  assert.deepEqual(list, [
    { name: "cost", description: "What this session cost", hint: "" },
    { name: "hello", description: "Say hello (project)", hint: "<name>" },
  ]);
  assert.deepEqual(commandList(undefined), []);
});

test("a turn's init says the model, the versions and the commands a page hides", () => {
  // Field names as SDK 0.3.311 sends them (recorded 2026-11-16).
  const init = {
    type: "system", subtype: "init", model: "claude-haiku-5-5", claude_code_version: "2.1.325", permissionMode: "auto",
    mcp_servers: [{ name: "claude-peers", status: "connected", source: "sdk" }], slash_commands: ["hello", "context"], terminal_slash_commands: ["doctor"],
  };
  assert.deepEqual(fromInit(init), {
    model: "claude-haiku-5-5", claudeCode: "2.1.325", permissionMode: "auto",
    mcp: [{ name: "claude-peers", status: "connected" }], terminalCommands: ["doctor"],
    commands: [{ name: "hello", description: "", hint: "" }, { name: "context", description: "", hint: "" }],
  });
});

test("bare names from init do not replace commands that came with descriptions", () => {
  const have = { commands: [{ name: "hello", description: "Say hello", hint: "" }] };
  assert.equal(fromInit({ slash_commands: ["hello", "x"] }, have).commands, undefined);
});

test("before any turn a query says its commands and the models to pick from", () => {
  const self = fromInitialization({
    commands: [{ name: "hello", description: "Say hello", argumentHint: "" }],
    models: [{ value: "default", displayName: "Default (recommended)", resolvedModel: "claude-opus-5-5" }, { value: "haiku", displayName: "Haiku 5.5", resolvedModel: "claude-haiku-5-5", supportsEffort: true, supportedEffortLevels: ["low", "high"] }],
  });
  assert.deepEqual(self.models, [{ value: "default", name: "Default (recommended)", model: "claude-opus-5-5" }, { value: "haiku", name: "Haiku 5.5", model: "claude-haiku-5-5", efforts: ["low", "high"] }]);
  assert.equal(self.commands[0].description, "Say hello");
  assert.deepEqual(fromInitialization({}), { commands: [], models: [] });
  assert.equal(fromInitialization({ claude_code_version: "2.1.326" }).claudeCode, "2.1.326");
});

test("a result is the turn's bill", () => {
  const r = resultOf({ total_cost_usd: 0.0123, duration_ms: 4100, num_turns: 3, usage: { input_tokens: 10, output_tokens: 200, cache_read_input_tokens: 3000, cache_creation_input_tokens: 40 } }, null, 1000);
  assert.deepEqual(r, { costUSD: 0.0123, durationMs: 4100, turns: 3, usage: { input: 10, output: 200, cacheRead: 3000, cacheWrite: 40 }, endedAt: 1000, cacheTtlMs: 0 });
  assert.deepEqual(resultOf({}).usage, { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 });
});

test("a result says how long its prompt cache lives, from what the turn wrote", () => {
  const hour = { usage: { cache_creation: { ephemeral_1h_input_tokens: 500, ephemeral_5m_input_tokens: 0 } } };
  assert.equal(resultOf(hour, null, 1).cacheTtlMs, 3600000);
  const five = { usage: { cache_creation: { ephemeral_5m_input_tokens: 80 } } };
  assert.equal(resultOf(five, null, 1).cacheTtlMs, 300000);
  const both = { usage: { cache_creation: { ephemeral_5m_input_tokens: 80, ephemeral_1h_input_tokens: 9 } } };
  assert.equal(resultOf(both, null, 1).cacheTtlMs, 3600000, "the hour wins: the long-lived prefix is what a cold start loses");
  // modelUsage is the session's running total (subagents and compaction
  // included), so an hour written once would read as an hour on every turn.
  const later = { usage: { cache_creation: { ephemeral_5m_input_tokens: 40 } }, modelUsage: { a: { cacheCreationEphemeral1hInputTokens: 9000 } } };
  assert.equal(resultOf(later, null, 1).cacheTtlMs, 300000, "this turn's own writes decide, not the running total");
  const prev = { endedAt: 500, cacheTtlMs: 3600000 };
  const read = resultOf({ usage: { cache_read_input_tokens: 9000 } }, prev, 900);
  assert.deepEqual([read.endedAt, read.cacheTtlMs], [900, 3600000], "a turn that only read the cache refreshes it with the lifetime it had");
  const none = resultOf({ is_error: true, usage: {} }, prev, 900);
  assert.deepEqual([none.endedAt, none.cacheTtlMs], [500, 3600000], "a turn that touched no cache (a failed call) leaves the countdown where it was");
  assert.deepEqual([resultOf({}, undefined, 900).endedAt, resultOf({}, undefined, 900).cacheTtlMs], [0, 0], "nothing known, nothing claimed");
});

test("context usage is the percent of the window in use", () => {
  assert.deepEqual(contextOf({ totalTokens: 84000, maxTokens: 200000, percentage: 42 }), { percent: 42, used: 84000, max: 200000 });
  assert.deepEqual(contextOf({}), { percent: 0, used: 0, max: 0 });
});

test("the advisor is the session's own, else the last settings file that names one", () => {
  const user = JSON.stringify({ advisorModel: "fable" }), project = JSON.stringify({ model: "opus" }), local = JSON.stringify({ advisorModel: "opus" });
  assert.deepEqual(advisorOf("", [user, project, ""]), { model: "fable", from: "settings", settings: "fable" });
  assert.deepEqual(advisorOf("", [user, project, local]), { model: "opus", from: "settings", settings: "opus" });
  assert.deepEqual(advisorOf("sonnet", [user]), { model: "sonnet", from: "session", settings: "fable" });
  const warned = [];
  assert.deepEqual(advisorOf("", ["{not json", project], (i) => warned.push(i)), { model: "", from: "", settings: "" }, "a file that does not parse names none");
  assert.deepEqual(warned, [0], "and is told, by its place");
  assert.deepEqual(advisorOf("", undefined), { model: "", from: "", settings: "" });
});

test("a background task keeps the time it was first seen, a new run starts its own, and a gone one is forgotten", () => {
  const seen = new Map();
  const dev = { task_id: "b1", run_id: "r1", task_type: "local_bash", description: "npm run dev" };
  const watch = { task_id: "w1", task_type: "monitor", description: "tail logs", ambient: true };
  assert.deepEqual(backgroundTasks([dev, watch], seen, 1000), [{ id: "b1", type: "local_bash", description: "npm run dev", agent: "", since: 1000 }], "an ambient watcher is not activity");
  const review = { task_id: "a1", task_type: "local_agent", subagent_type: "qa:code-reviewer", description: "review" };
  assert.deepEqual(backgroundTasks([dev, review], seen, 5000).map((t) => [t.id, t.since]), [["b1", 1000], ["a1", 5000]], "the dev server keeps its start");
  assert.deepEqual(backgroundTasks([{ ...dev, run_id: "r2" }], seen, 9000).map((t) => [t.id, t.since]), [["b1", 9000]], "a resumed task is a new run");
  assert.deepEqual([...seen.keys()], ["r2"], "what left the set is forgotten");
  assert.deepEqual(backgroundTasks(undefined, seen, 1), []);
});

test("the effort the settings give a model: the environment, then the files in order, a model's own line before the general one", () => {
  const user = JSON.stringify({ effortLevel: "high", modelSettings: { "claude-opus-5-5": { effortLevel: "medium" } } });
  assert.equal(settingsEffort("claude-opus-5-5", [user, "", ""]), "medium");
  assert.equal(settingsEffort("claude-opus-5-5[1m]", [user]), "medium", "the context suffix is not part of the model");
  assert.equal(settingsEffort("claude-fable-5-2", [user]), "high", "no line of its own: the general one");
  assert.equal(settingsEffort("claude-opus-5-5", [user, JSON.stringify({ effortLevel: "low" })]), "medium", "a model's own line wins over a later general one");
  assert.equal(settingsEffort("claude-opus-5-5", [user, "", JSON.stringify({ modelSettings: { "claude-opus-5-5": { effortLevel: "xhigh" } } })]), "xhigh", "a later file wins");
  assert.equal(settingsEffort("claude-opus-5-5", [user], { CLAUDE_CODE_EFFORT_LEVEL: "max" }), "max", "the environment wins");
  assert.equal(settingsEffort("claude-opus-5-5", ["{oops", ""]), "", "nothing readable: unknown");
  assert.equal(settingsEffort("", [user]), "high");
});
