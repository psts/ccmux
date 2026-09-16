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
//   serve.mjs serve --port N --agent NAME --plugin-dir DIR --system-prompt-file FILE
//             [--add-dir DIR]... [--model M] [--allowed-tools a,b] [--disallowed-tools c]
//             [--session ID]
//   serve.mjs sessions --dir DIR                 conversations opened in DIR, JSON
//   serve.mjs export --dir DIR --session ID      one conversation, JSON
//
// The daemon's pane identity (CCMUX_DAEMON_URL, CCMUX_PANE_ID) comes from the
// pane environment, like the opencode plugin's; busy/idle/needs-input signals
// go there so the tab badge, the sidebar flash and the lifecycle clock work.
import { readFileSync } from "node:fs";
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

async function cmdServe(o) {
  if (!o.port) throw new Error("--port is required");
  const agent = new Agent(o, {
    query, listSessions, getSessionMessages,
    cwd: process.cwd(), env: process.env, readFile: readFileSync, fetch: globalThis.fetch,
    out: (s) => process.stdout.write(s), err: (s) => process.stderr.write(s),
  });
  await agent.preload();
  const server = createAgentServer(agent);
  server.listen(o.port, "127.0.0.1", () => agent.log(`⚙ ${o.agent || "agent"} · chat on port ${o.port} · type here or in the chat · ctrl-d ends the session`));
  const rl = createInterface({ input: process.stdin, terminal: false });
  rl.on("line", (line) => {
    if (!line.trim()) return;
    try { agent.prompt(line.trim()); } catch (err) { agent.log("! " + (err && err.message ? err.message : String(err))); }
  });
  const end = () => agent.shutdown().finally(() => process.exit(0));
  rl.on("close", end);
  process.on("SIGTERM", end);
  process.on("SIGINT", end);
  if (o.session) agent.log("⚙ continuing conversation " + o.session.slice(0, 8));
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
