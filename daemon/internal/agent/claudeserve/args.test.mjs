// node --test args.test.mjs — the flag set the daemon writes (launch.go's
// claudeServeLine, asserted in launch_test.go) read back the way serve.mjs
// reads it.
import { test } from "node:test";
import assert from "node:assert/strict";
import { parseArgs } from "./args.mjs";

test("the full launch line the daemon writes parses into the sidecar's options", () => {
  const o = parseArgs([
    "--port", "41235", "--agent", "kb-writer",
    "--plugin-dir", "/home/u/.ccmux/agents/kb-writer",
    "--system-prompt-file", "/home/u/.ccmux/agents/kb-writer/AGENTS.md",
    "--mcp-file", "/home/u/.ccmux/agents/kb-writer/mcp.json",
    "--add-dir", "/repo", "--add-dir", "/repo2", "--add-dir", "/srv/kb",
    "--model", "opus",
    "--allowed-tools", "Bash(git log *),Glob,Grep,Read",
    "--disallowed-tools", "Edit,WebFetch,WebSearch,Write",
    "--session", "0f0e0d0c-1111-2222-3333-444444444444",
  ]);
  assert.equal(o.port, 41235);
  assert.equal(o.agent, "kb-writer");
  assert.deepEqual(o.pluginDirs, ["/home/u/.ccmux/agents/kb-writer"]);
  assert.equal(o.systemPromptFile, "/home/u/.ccmux/agents/kb-writer/AGENTS.md");
  assert.deepEqual(o.mcpFiles, ["/home/u/.ccmux/agents/kb-writer/mcp.json"]);
  assert.deepEqual(o.addDirs, ["/repo", "/repo2", "/srv/kb"]);
  assert.equal(o.model, "opus");
  assert.deepEqual(o.allowed, ["Bash(git log *)", "Glob", "Grep", "Read"]);
  assert.deepEqual(o.disallowed, ["Edit", "WebFetch", "WebSearch", "Write"]);
  assert.equal(o.session, "0f0e0d0c-1111-2222-3333-444444444444");
});

test("a minimal line leaves the lists empty and the optional flags unset", () => {
  const o = parseArgs(["--port", "7", "--agent", "probe"]);
  assert.deepEqual(o, { addDirs: [], allowed: [], disallowed: [], writeRoots: [], pluginDirs: [], mcpFiles: [], shellBoxWrite: [], port: 7, agent: "probe" });
  assert.deepEqual(parseArgs(["--allowed-tools", ""]).allowed, [], "an empty list is no tools, not one empty name");
  assert.deepEqual(parseArgs([]), { addDirs: [], allowed: [], disallowed: [], writeRoots: [], pluginDirs: [], mcpFiles: [], shellBoxWrite: [] });
});

test("the offline verbs' flags", () => {
  assert.deepEqual(parseArgs(["--dir", "/inst", "--session", "s1"]), { addDirs: [], allowed: [], disallowed: [], writeRoots: [], pluginDirs: [], mcpFiles: [], shellBoxWrite: [], dir: "/inst", session: "s1" });
});

test("a flag the daemon and the sidecar disagree on fails at once", () => {
  assert.throws(() => parseArgs(["--allowedTools", "Read"]), /unknown flag --allowedTools/);
  assert.throws(() => parseArgs(["--port"]), /--port needs a value/);
  assert.throws(() => parseArgs(["--port", "1", "--agent"]), /--agent needs a value/);
});

test("an isolated start's flags: repeated plugin and mcp files, setting sources, the peers shim, skills", () => {
  const o = parseArgs([
    "--plugin-dir", "/agents/radar", "--plugin-dir", "/plugins/kb",
    "--mcp-file", "/agents/radar/mcp.json", "--mcp-file", "/agents/.ccmux/caps/radar.mcp.json",
    "--setting-sources", "project", "--peers-command", "/bin/ccmux-peers", "--skills", "radar:scan,kb:draft",
  ]);
  assert.deepEqual(o.pluginDirs, ["/agents/radar", "/plugins/kb"]);
  assert.deepEqual(o.mcpFiles, ["/agents/radar/mcp.json", "/agents/.ccmux/caps/radar.mcp.json"]);
  assert.deepEqual(o.settingSources, ["project"]);
  assert.equal(o.peersCommand, "/bin/ccmux-peers");
  assert.deepEqual(o.skills, ["radar:scan", "kb:draft"]);
  assert.deepEqual(parseArgs(["--skills", ""]).skills, [], "every skill off is an empty list, not unset");
});

test("a session runner's line, as internal/runner's BuildSpec writes it", () => {
  const o = parseArgs([
    "--socket", "/run/user/1/ccmuxd/s/abc.sock", "--state", "/st/abc.state.json", "--session-auto", "abc",
    "--detached", "1", "--setting-sources", "user,project,local", "--agent", "app", "--claude", "/home/u/.local/bin/claude",
  ]);
  assert.equal(o.socket, "/run/user/1/ccmuxd/s/abc.sock");
  assert.equal(o.state, "/st/abc.state.json");
  assert.equal(o.sessionAuto, "abc");
  assert.equal(o.detached, true, "a runner does not end on closed input");
  assert.equal(o.claude, "/home/u/.local/bin/claude");
  assert.deepEqual(o.settingSources, ["user", "project", "local"]);
  assert.equal(o.port, undefined);
  assert.equal(o.session, undefined, "--session-auto is not --session: it opens fresh when nothing is on disk");
  assert.equal(parseArgs(["--detached", "0"]).detached, false, "only 1 detaches");
  assert.equal(o.loadout, undefined, "no loadout named, none read");
  assert.equal(parseArgs(["--socket", "/s.sock", "--loadout", "/st/abc.loadout.json"]).loadout, "/st/abc.loadout.json");
});

test("a thread member's line: read only in its repo's folder, or kept out of it in its worktree", () => {
  const reading = parseArgs(["--socket", "/s.sock", "--read-only", "1", "--system-prompt-file", "/st/abc.brief.md"]);
  assert.equal(reading.readOnly, true);
  assert.equal(reading.keepOut, undefined);
  assert.equal(reading.systemPromptFile, "/st/abc.brief.md");
  const working = parseArgs(["--socket", "/s.sock", "--keep-out", "/r/api"]);
  assert.equal(working.keepOut, "/r/api");
  assert.equal(working.readOnly, undefined);
  assert.equal(parseArgs(["--read-only", "0"]).readOnly, false, "only 1 is read only");
});

test("a task builder's line: the file fence, its effort and its permission mode", () => {
  const b = parseArgs(["--socket", "/s.sock", "--model", "opus", "--effort", "medium", "--permission-mode", "auto", "--keep-out-files", "/r/api"]);
  assert.equal(b.effort, "medium");
  assert.equal(b.permissionMode, "auto");
  assert.equal(b.keepOutFiles, "/r/api");
  assert.equal(b.keepOut, undefined, "not the full fence");
  const plain = parseArgs(["--socket", "/s.sock"]);
  assert.equal(plain.effort, undefined);
  assert.equal(plain.permissionMode, undefined);
});
