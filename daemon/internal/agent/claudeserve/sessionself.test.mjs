// node --test sessionself.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { commandList, fromInit, fromInitialization, resultOf } from "./sessionself.mjs";

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
    models: [{ value: "default", displayName: "Default (recommended)", resolvedModel: "claude-opus-5-5" }, { value: "haiku", displayName: "Haiku 5.5", resolvedModel: "claude-haiku-5-5", supportsEffort: true }],
  });
  assert.deepEqual(self.models, [{ value: "default", name: "Default (recommended)", model: "claude-opus-5-5" }, { value: "haiku", name: "Haiku 5.5", model: "claude-haiku-5-5" }]);
  assert.equal(self.commands[0].description, "Say hello");
  assert.deepEqual(fromInitialization({}), { commands: [], models: [] });
  assert.equal(fromInitialization({ claude_code_version: "2.1.326" }).claudeCode, "2.1.326");
});

test("a result is the turn's bill", () => {
  const r = resultOf({ total_cost_usd: 0.0123, duration_ms: 4100, num_turns: 3, usage: { input_tokens: 10, output_tokens: 200, cache_read_input_tokens: 3000, cache_creation_input_tokens: 40 } });
  assert.deepEqual(r, { costUSD: 0.0123, durationMs: 4100, turns: 3, usage: { input: 10, output: 200, cacheRead: 3000, cacheWrite: 40 } });
  assert.deepEqual(resultOf({}).usage, { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 });
});
