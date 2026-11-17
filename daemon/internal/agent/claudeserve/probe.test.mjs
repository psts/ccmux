import { test } from "node:test";
import assert from "node:assert/strict";
import { summarizeInit, parseArgs } from "./probe.mjs";

test("summarizeInit keeps plugins, servers and only the MCP tools", () => {
  const got = summarizeInit({
    plugins: [{ name: "qa", path: "/p/qa", source: "qa@personal", version: "0.8.0" }, { name: "cc-plugin-x", path: "builtin", source: "cc-plugin-x@builtin" }],
    mcp_servers: [{ name: "claude.ai Gmail", status: "connected", source: "claudeai" }, { name: "claude-peers", status: "connected" }],
    tools: ["Bash", "mcp__claude_ai_Gmail__search", 7],
    skills: ["qa:check"],
  });
  assert.deepEqual(got, {
    plugins: [{ name: "qa", path: "/p/qa", source: "qa@personal", version: "0.8.0" }, { name: "cc-plugin-x", path: "builtin", source: "cc-plugin-x@builtin", version: "" }],
    mcpServers: [{ name: "claude.ai Gmail", source: "claudeai", status: "connected" }, { name: "claude-peers", source: "", status: "connected" }],
    tools: ["mcp__claude_ai_Gmail__search"],
  });
});

test("summarizeInit of an empty init is three empty lists", () => {
  assert.deepEqual(summarizeInit({}), { plugins: [], mcpServers: [], tools: [] });
});

test("parseArgs takes the known flags and wants a cwd", () => {
  assert.deepEqual(parseArgs(["--cwd", "/tmp/x", "--claude", "/bin/claude"]), { cwd: "/tmp/x", claude: "/bin/claude", model: "haiku" });
  assert.throws(() => parseArgs(["--claude", "/bin/claude"]), /cwd/);
  assert.throws(() => parseArgs(["--nope", "1", "--cwd", "/x"]), /unknown/);
});
