// What a session says of itself, for its page: the model it runs on, the
// slash commands and models it offers, its MCP servers, and what a turn
// cost. Read off the SDK's own messages; pure (sessionself.test.mjs).

// commandList is the SDK's slash commands as the page lists them. Rows can
// share a name (a built-in and a plugin's): the first one stands, which is
// the one /name runs.
export function commandList(commands) {
  const seen = new Set();
  const out = [];
  for (const c of commands || []) {
    const name = String((c && c.name) || "");
    if (!name || seen.has(name)) continue;
    seen.add(name);
    out.push({ name, description: String(c.description || ""), hint: String(c.argumentHint || "") });
  }
  return out;
}

// fromInit is what the init message at the start of a turn says: the model
// that turn runs on, the versions, the permission mode, the MCP servers, and
// the commands that only make sense in a terminal (the page hides those).
// have is what the session already said: the bare names here must not
// replace a command list that came with descriptions.
export function fromInit(m, have = {}) {
  const self = {
    model: String(m.model || ""),
    claudeCode: String(m.claude_code_version || ""),
    permissionMode: String(m.permissionMode || ""),
    mcp: (m.mcp_servers || []).map((s) => ({ name: String(s.name || ""), status: String(s.status || "") })),
    terminalCommands: (m.terminal_slash_commands || []).map(String),
  };
  if (!have.commands || !have.commands.length) self.commands = commandList((m.slash_commands || []).map((name) => ({ name })));
  return self;
}

// fromInitialization is what a query says before any turn: its commands
// with their descriptions, and the models this login may pick from.
export function fromInitialization(r) {
  return {
    commands: commandList(r.commands),
    models: (r.models || []).map((x) => ({ value: String(x.value || ""), name: String(x.displayName || x.value || ""), model: String(x.resolvedModel || "") })),
  };
}

// resultOf is a finished turn's bill: what the session has cost so far, how
// long the turn took and the tokens it used.
export function resultOf(m) {
  const u = m.usage || {};
  return {
    costUSD: Number(m.total_cost_usd) || 0,
    durationMs: Number(m.duration_ms) || 0,
    turns: Number(m.num_turns) || 0,
    usage: {
      input: Number(u.input_tokens) || 0, output: Number(u.output_tokens) || 0,
      cacheRead: Number(u.cache_read_input_tokens) || 0, cacheWrite: Number(u.cache_creation_input_tokens) || 0,
    },
  };
}
