// The sidecar's command line, as the daemon writes it: the serve verb from
// agent/launch.go (claudeServeLine, and its caller for --session; asserted
// in launch_test.go), the sessions and export verbs from agent/claudeserve.go
// (ClaudeOfflineSessions, ClaudeOfflineTranscript; their stub test reads
// values by position, not flag name). One flag table, tested here against
// the same lines. No SDK import.
//
//   serve   --port N --agent NAME --plugin-dir DIR... --system-prompt-file FILE
//           --mcp-file FILE... [--add-dir DIR]... [--model M] [--allowed-tools a,b]
//           [--disallowed-tools c] [--write-root DIR]... [--write-inside allow]
//           [--setting-sources a,b] [--peers-command CMD] [--skills a,b]
//           [--session ID]
//   sessions --dir DIR
//   export   --dir DIR --session ID

const list = (s) => s.split(",").filter(Boolean);

// flags maps each flag onto what it sets; every flag takes one value.
const flags = {
  "--port": (o, v) => { o.port = Number(v); },
  "--agent": (o, v) => { o.agent = v; },
  "--plugin-dir": (o, v) => { o.pluginDirs.push(v); },
  "--system-prompt-file": (o, v) => { o.systemPromptFile = v; },
  "--mcp-file": (o, v) => { o.mcpFiles.push(v); },
  "--add-dir": (o, v) => { o.addDirs.push(v); },
  "--model": (o, v) => { o.model = v; },
  "--allowed-tools": (o, v) => { o.allowed = list(v); },
  "--disallowed-tools": (o, v) => { o.disallowed = list(v); },
  "--write-root": (o, v) => { o.writeRoots.push(v); },
  "--write-inside": (o, v) => { o.writeInside = v; },
  "--setting-sources": (o, v) => { o.settingSources = list(v); },
  "--peers-command": (o, v) => { o.peersCommand = v; },
  "--skills": (o, v) => { o.skills = list(v); },
  "--session": (o, v) => { o.session = v; },
  "--dir": (o, v) => { o.dir = v; },
};

// parseArgs reads the flags after the verb. An unknown flag, or a flag with
// no value, throws: a launch line the daemon and the sidecar disagree on
// must fail at the start, where the pane shows why, not later.
export function parseArgs(argv) {
  const o = { addDirs: [], allowed: [], disallowed: [], writeRoots: [], pluginDirs: [], mcpFiles: [] };
  for (let i = 0; i < argv.length; i += 2) {
    const set = flags[argv[i]];
    if (!set) throw new Error("unknown flag " + argv[i]);
    if (i + 1 >= argv.length) throw new Error(argv[i] + " needs a value");
    set(o, argv[i + 1]);
  }
  return o;
}
