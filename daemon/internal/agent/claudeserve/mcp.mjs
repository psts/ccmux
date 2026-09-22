// The base's MCP servers for the SDK. The daemon hands the sidecar the
// base's mcp.json (--mcp-file) in the harness-neutral shape agent/mcp.go
// writes: a local server is {command, args, env}, a remote one is
// {type: "http" | "sse", url, headers}. This maps that onto the SDK's
// mcpServers option, expands ${VAR} from the process
// environment (the instance's .env rides in on the launch line, so a base
// holds no secrets), and names the permission rules that let every server's
// tools run without a card: the base chose the server, so its tools are
// wanted; the peers shim is always among them. Pure: no I/O, no SDK import.

const ref = /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g;

// expand replaces ${VAR} with env[VAR]. A reference to an unset variable
// stays as written (and is reported by mcpFromNeutral), so a typo shows up
// in the server's error rather than as "". There is no default form: the
// opencode harness could not honour one, and a base means the same on both.
export function expand(s, env) {
  return String(s).replace(ref, (whole, name) => (env[name] !== undefined ? env[name] : whole));
}

function expandMap(m, env) {
  const out = {};
  for (const [k, v] of Object.entries(m || {})) out[k] = expand(v, env);
  return out;
}

// sdkServer renders one neutral entry in the SDK's shape.
function sdkServer(e, env) {
  if (e.url) {
    const s = { type: e.type === "sse" ? "sse" : "http", url: expand(e.url, env) };
    if (e.headers && Object.keys(e.headers).length) s.headers = expandMap(e.headers, env);
    return s;
  }
  const s = { type: "stdio", command: expand(e.command, env), args: (e.args || []).map((a) => expand(a, env)) };
  if (e.env && Object.keys(e.env).length) s.env = expandMap(e.env, env);
  return s;
}

// PEERS_RULE allows every tool of the peers shim (mcp__<server> is Claude
// Code's "all tools of that server" rule). The shim is not in mcp.json: it
// reaches the Claude child through the user's own config, but its tools
// are the agent's voice on the bus and never a question for a human.
export const PEERS_RULE = "mcp__claude-peers";

// unresolvedRefs lists the ${VAR} references still in a rendered server:
// the variables the instance's .env does not set.
function unresolvedRefs(server) {
  const names = new Set();
  for (const m of JSON.stringify(server).matchAll(ref)) names.add(m[1]);
  return [...names];
}

// mcpFromNeutral parses the mcp.json text ("" or "{}" means none) and
// returns the SDK servers by name, the allow rules for them and the peers
// shim, and per server the variables it references that are not set (the
// caller says so in the terminal: the SDK reports only "failed" for a
// server whose url or token came out wrong). A file that is not a JSON
// object throws: a base that cannot be read must fail the start where the
// pane shows why.
export function mcpFromNeutral(text, env) {
  const neutral = text && text.trim() ? JSON.parse(text) : {};
  if (neutral === null || typeof neutral !== "object" || Array.isArray(neutral)) throw new Error("mcp.json must be a JSON object of servers");
  const servers = {};
  const allow = [PEERS_RULE];
  const unresolved = {};
  for (const [name, e] of Object.entries(neutral)) {
    if (!e || typeof e !== "object" || (!e.command && !e.url)) throw new Error(`mcp.json: server "${name}" needs a command or a url`);
    servers[name] = sdkServer(e, env);
    allow.push(`mcp__${name}`);
    const missing = unresolvedRefs(servers[name]);
    if (missing.length) unresolved[name] = missing;
  }
  return { servers, allow, unresolved };
}
