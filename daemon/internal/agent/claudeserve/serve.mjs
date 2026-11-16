#!/usr/bin/env node
// ccmux-claude-serve: runs a Claude Code agent through the Claude Agent SDK
// and serves the conversation on a loopback port in the shape the daemon's
// chat view already reads from opencode (agent/opencodeclient.go), so an
// agent on the claude harness gets the same chat as one on opencode.
//
// Lives in the agent's tmux pane the way opencode's TUI does: what prints
// here is the terminal view behind the chat's toggle (prompts, replies,
// tool calls), a typed line is a prompt, and end-of-input (ctrl-d) ends
// the session — which is how the lifecycle puts an idle agent to sleep.
//
//   serve.mjs serve --port N --agent NAME --plugin-dir DIR... --system-prompt-file FILE
//             --mcp-file FILE... [--add-dir DIR]... [--model M] [--allowed-tools a,b]
//             [--disallowed-tools c] [--setting-sources a,b] [--peers-command CMD]
//             [--skills a,b] [--session ID]   (args.mjs has them all)
//   serve.mjs sessions --dir DIR                 conversations opened in DIR, JSON
//   serve.mjs export --dir DIR --session ID      one conversation, JSON
//
// The daemon's pane identity (CCMUX_DAEMON_URL, CCMUX_PANE_ID) comes from the
// pane environment, like the opencode plugin's; busy/idle/permission/question signals
// go there so the tab badge, the sidebar flash and the lifecycle clock work.
import { readFileSync, writeFileSync, renameSync, rmSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";
import { createInterface } from "node:readline";
import { query, listSessions, getSessionMessages } from "@anthropic-ai/claude-agent-sdk";
import { Transcript, sessionInfo } from "./translate.mjs";
import { createAgentServer } from "./routes.mjs";
import { Agent } from "./agent.mjs";
import { parseArgs } from "./args.mjs";

// --- offline reads: the daemon calls these for an asleep agent ---

async function cmdSessions(o) {
  const list = await listSessions({ dir: o.dir });
  process.stdout.write(JSON.stringify(list.map(sessionInfo)) + "\n");
}

async function cmdExport(o) {
  const msgs = await getSessionMessages(o.session, { dir: o.dir });
  const t = new Transcript(o.session);
  for (const m of msgs) t.apply(m);
  process.stdout.write(JSON.stringify({ messages: t.messages() }) + "\n");
}

// The SDK warns that tools on the allow list never reach canUseTool. That
// is what the base's "allow" gate means, so the warning is noise here.
process.removeAllListeners("warning");
process.on("warning", (w) => {
  if (w.code !== "CLAUDE_SDK_CAN_USE_TOOL_SHADOWED") process.stderr.write(`${w.name}: ${w.message}\n`);
});

// sdkVersion is the installed SDK's version, said in a runner's hello.
function sdkVersion() {
  try {
    return JSON.parse(readFileSync(new URL("./node_modules/@anthropic-ai/claude-agent-sdk/package.json", import.meta.url), "utf8")).version || "";
  } catch {
    return "";
  }
}

// listen puts the server on the runner's unix socket, or on the agent
// pane's loopback port. A socket left by a runner that died is removed
// first; systemd runs one runner per session, so it cannot be a live one.
function listen(server, o, ready) {
  if (!o.socket) return server.listen(o.port, "127.0.0.1", ready);
  mkdirSync(dirname(o.socket), { recursive: true, mode: 0o700 });
  rmSync(o.socket, { force: true });
  return server.listen(o.socket, ready);
}

async function cmdServe(o) {
  if (!o.port && !o.socket) throw new Error("--port or --socket is required");
  const agent = new Agent(o, {
    query, listSessions, getSessionMessages, sdkVersion: sdkVersion(),
    writeFile: (p, t) => writeFileSync(p, t, { mode: 0o600 }), rename: renameSync,
    exit: (code) => process.exit(code),
    cwd: process.cwd(), env: process.env, readFile: readFileSync, fetch: globalThis.fetch,
    out: (s) => process.stdout.write(s), err: (s) => process.stderr.write(s),
  });
  await agent.preload();
  // The Claude child (and the peers shim inside it) comes up now, so the
  // agent is on the bus from its first second, not from its first prompt.
  agent.ensureQuery();
  agent.recover();
  const server = createAgentServer(agent);
  const where = o.socket ? `socket ${o.socket}` : `chat on port ${o.port}`;
  listen(server, o, () => agent.log(`⚙ ${o.agent || "session"} · ${where}${o.detached ? "" : " · type here or in the chat · ctrl-d ends the session"}`));
  const end = () => agent.shutdown().finally(() => process.exit(0));
  process.on("SIGTERM", end);
  process.on("SIGINT", end);
  if (o.session) agent.log("⚙ continuing conversation " + o.session.slice(0, 8));
  // A runner under systemd has no terminal: its input is /dev/null, which
  // closes at once, so only an agent pane ends on end-of-input.
  if (o.detached) return;
  const rl = createInterface({ input: process.stdin, terminal: false });
  rl.on("line", (line) => {
    if (!line.trim()) return;
    try { agent.prompt(line.trim()); } catch (err) { agent.log("! " + (err && err.message ? err.message : String(err))); }
  });
  rl.on("close", end);
}

async function main() {
  const [cmd, ...rest] = process.argv.slice(2);
  const o = parseArgs(rest);
  switch (cmd) {
    case "serve": return cmdServe(o);
    case "sessions": return cmdSessions(o);
    case "export": return cmdExport(o);
    default: throw new Error("usage: serve.mjs serve|sessions|export ...");
  }
}

main().catch((err) => {
  process.stderr.write((err && err.message ? err.message : String(err)) + "\n");
  process.exit(1);
});
