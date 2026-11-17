// probe.mjs: start one throwaway Claude Code session with the host user's
// own setup, print what its init message says it loaded, and stop. The
// daemon's catalog import runs it (capprobe.go): init is the ground truth
// of what a thread session gets, claude.ai's synced plugins and connectors
// included, which no file on disk lists. Init only comes with a first
// prompt, so it sends "hi" on haiku and aborts as soon as init arrives,
// before the model answers.
//
//   node probe.mjs --cwd <empty folder> --claude <path to claude>
//
// Prints one JSON line: {plugins, mcpServers, tools} (summarizeInit).
import { pathToFileURL } from "node:url";

// summarizeInit keeps what the catalog reads from an init message: every
// plugin with its folder, source and version, every MCP server with where
// it comes from, and the MCP tools' names, which the cost estimate counts.
export function summarizeInit(m) {
  const str = (v) => (typeof v === "string" ? v : "");
  return {
    plugins: (m.plugins || []).map((p) => ({ name: str(p.name), path: str(p.path), source: str(p.source), version: str(p.version) })),
    mcpServers: (m.mcp_servers || []).map((s) => ({ name: str(s.name), source: str(s.source), status: str(s.status) })),
    tools: (m.tools || []).filter((t) => typeof t === "string" && t.startsWith("mcp__")),
  };
}

export function parseArgs(argv) {
  const o = { cwd: "", claude: "", model: "haiku" };
  for (let i = 0; i < argv.length; i += 2) {
    const k = argv[i].replace(/^--/, "");
    if (!(k in o) || argv[i + 1] === undefined) throw new Error(`probe: unknown or empty flag ${argv[i]}`);
    o[k] = argv[i + 1];
  }
  if (!o.cwd) throw new Error("probe: --cwd is required");
  return o;
}

async function main() {
  const o = parseArgs(process.argv.slice(2));
  const { query } = await import("@anthropic-ai/claude-agent-sdk");
  const abort = new AbortController();
  async function* prompt() {
    yield { type: "user", message: { role: "user", content: "hi" }, parent_tool_use_id: null };
    await new Promise(() => {});
  }
  const options = { cwd: o.cwd, settingSources: ["user", "project", "local"], abortController: abort, persistSession: false, model: o.model };
  if (o.claude) options.pathToClaudeCodeExecutable = o.claude;
  for await (const m of query({ prompt: prompt(), options })) {
    if (m.type === "system" && m.subtype === "init") {
      process.stdout.write(JSON.stringify(summarizeInit(m)) + "\n");
      abort.abort();
      return;
    }
    if (m.type === "result") throw new Error("probe: the session ended before its init message");
  }
  throw new Error("probe: no init message");
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().then(() => process.exit(0), (e) => { process.stderr.write(String(e && e.message || e) + "\n"); process.exit(1); });
}
