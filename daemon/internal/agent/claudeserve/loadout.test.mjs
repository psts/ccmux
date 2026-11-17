// node --test loadout.test.mjs — the loadout file the daemon writes
// (agent/loadout.go's Loadout) read and put onto SDK options.
import { test } from "node:test";
import assert from "node:assert/strict";
import { parseLoadout, applyLoadout, describeLoadout } from "./loadout.mjs";

const empty = { pluginDirs: [], enabledPlugins: {}, skills: [], deniedServers: [], connectors: false, mcp: {} };
const text = (over = {}) => JSON.stringify({ ...empty, ...over });

test("a loadout file reads back field for field", () => {
  const lo = parseLoadout(text({
    pluginDirs: ["/caps/personal/plugins/kb"], enabledPlugins: { "qa@personal": true, "stripe@official": false },
    skills: ["qa:check", "glitchtip"], deniedServers: ["claude.ai Gmail", "claude_design"], connectors: true,
    mcp: { search: { type: "http", url: "https://s/mcp" } },
  }));
  assert.deepEqual(lo.pluginDirs, ["/caps/personal/plugins/kb"]);
  assert.deepEqual(lo.enabledPlugins, { "qa@personal": true, "stripe@official": false });
  assert.deepEqual(lo.skills, ["qa:check", "glitchtip"]);
  assert.deepEqual(lo.deniedServers, ["claude.ai Gmail", "claude_design"]);
  assert.equal(lo.connectors, true);
  assert.deepEqual(Object.keys(lo.mcp), ["search"]);
});

test("a file with a field missing or of the wrong kind fails the start, never loads everything", () => {
  for (const field of ["pluginDirs", "enabledPlugins", "skills", "deniedServers", "connectors", "mcp"]) {
    const raw = { ...empty };
    delete raw[field];
    assert.throws(() => parseLoadout(JSON.stringify(raw)), new RegExp(field), `${field} missing`);
  }
  assert.throws(() => parseLoadout(text({ skills: null })), /skills must be a list/, "Go's nil slice is null: not a list");
  assert.throws(() => parseLoadout(text({ skills: "qa:check" })), /skills must be a list/);
  assert.throws(() => parseLoadout(text({ skills: [1] })), /skills must be a list/);
  assert.throws(() => parseLoadout(text({ enabledPlugins: { "qa@personal": "yes" } })), /true or false/);
  assert.throws(() => parseLoadout("[]"), /JSON object/);
  assert.throws(() => parseLoadout("not json"));
});

test("the bus is never denied and never replaced, whatever the file says", () => {
  const lo = parseLoadout(text({ deniedServers: ["claude-peers", "claude_design"], mcp: { "claude-peers": { command: "evil" }, search: { url: "https://s" } } }));
  assert.deepEqual(lo.deniedServers, ["claude_design"]);
  assert.deepEqual(Object.keys(lo.mcp), ["search"]);
  const opts = applyLoadout({}, lo, {});
  assert.deepEqual(opts.settings.deniedMcpServers, [{ serverName: "claude_design" }]);
});

test("nothing chosen: no skill is offered, connectors are off, and the lists stay lists", () => {
  const opts = applyLoadout({ cwd: "/r" }, parseLoadout(text()), { HOME: "/h" });
  assert.deepEqual(opts.skills, [], "an empty list, which offers no skill; undefined would offer every skill");
  assert.equal(opts.env.ENABLE_CLAUDEAI_MCP_SERVERS, "false");
  assert.equal(opts.env.HOME, "/h", "the rest of the environment is kept");
  assert.equal(opts.plugins, undefined);
  assert.equal(opts.settings, undefined, "nothing to switch, so no settings of its own");
  assert.equal(opts.strictMcpConfig, undefined, "never: it would drop the bus");
});

test("a full loadout maps onto plugins, settings, skills and the connector switch", () => {
  const lo = parseLoadout(text({
    pluginDirs: ["/caps/kb"], enabledPlugins: { "qa@personal": true, "marketing@synced": false },
    skills: ["qa:check"], deniedServers: ["claude.ai Gmail"], connectors: true,
  }));
  const opts = applyLoadout({ plugins: [{ type: "local", path: "/base" }], env: { A: "1" } }, lo, { B: "2" });
  assert.deepEqual(opts.plugins, [{ type: "local", path: "/base" }, { type: "local", path: "/caps/kb" }]);
  assert.deepEqual(opts.settings, { enabledPlugins: { "qa@personal": true, "marketing@synced": false }, deniedMcpServers: [{ serverName: "claude.ai Gmail" }] });
  assert.deepEqual(opts.skills, ["qa:check"]);
  assert.deepEqual(opts.env, { A: "1" }, "a chosen connector leaves the environment as it was");
});

test("an environment set earlier is kept when connectors go off", () => {
  const opts = applyLoadout({ env: { A: "1" } }, parseLoadout(text()), { B: "2" });
  assert.deepEqual(opts.env, { A: "1", ENABLE_CLAUDEAI_MCP_SERVERS: "false" });
});

test("the start line says what the session was given", () => {
  const line = describeLoadout(parseLoadout(text({ pluginDirs: ["/caps/kb"], enabledPlugins: { a: true, b: false, c: false }, skills: ["x"], deniedServers: ["s"] })));
  assert.match(line, /2 plugin\(s\) on, 2 off, 1 skill\(s\) offered, 1 server\(s\) denied, 0 catalog server\(s\), claude\.ai connectors off/);
});
