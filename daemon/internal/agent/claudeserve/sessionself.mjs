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
// with their descriptions, the models this login may pick from, and Claude
// Code's version when it says it.
export function fromInitialization(r) {
  const self = r.claude_code_version ? { claudeCode: String(r.claude_code_version) } : {};
  return {
    ...self,
    commands: commandList(r.commands),
    models: (r.models || []).map(modelOf),
  };
}

// resultOf is a finished turn's bill: what the session has cost so far, how
// long the turn took and the tokens it used, plus when the prompt cache was
// last touched and how long it lives. prev is the turn before's result. A
// turn that read the cache but wrote none refreshes it with the lifetime it
// had; one that touched no cache at all (a call that failed) leaves both as
// they were, so the page does not count a cold cache down as warm. now is
// the runner's clock.
export function resultOf(m, prev, now) {
  const u = m.usage || {};
  const touched = (Number(u.cache_read_input_tokens) || 0) + (Number(u.cache_creation_input_tokens) || 0) > 0 || cacheTtlOf(m) > 0;
  const p = prev || {};
  return {
    costUSD: Number(m.total_cost_usd) || 0,
    durationMs: Number(m.duration_ms) || 0,
    turns: Number(m.num_turns) || 0,
    usage: {
      input: Number(u.input_tokens) || 0, output: Number(u.output_tokens) || 0,
      cacheRead: Number(u.cache_read_input_tokens) || 0, cacheWrite: Number(u.cache_creation_input_tokens) || 0,
    },
    endedAt: touched ? (now === undefined ? Date.now() : now) : Number(p.endedAt) || 0,
    cacheTtlMs: (touched && cacheTtlOf(m)) || Number(p.cacheTtlMs) || 0,
  };
}

const HOUR = 3600000, FIVE_MIN = 300000;

// cacheTtlOf is the lifetime of the cache a turn wrote: an hour if any of it
// was written for an hour, five minutes if only for five, 0 if it wrote none.
// It reads the turn's own usage (the main loop), never modelUsage: that is
// the session's running total, subagents and compaction included, so an
// hour written once would read as an hour on every later turn.
function cacheTtlOf(m) {
  const c = (m.usage && m.usage.cache_creation) || {};
  const hour = Number(c.ephemeral_1h_input_tokens) || 0, five = Number(c.ephemeral_5m_input_tokens) || 0;
  return hour > 0 ? HOUR : five > 0 ? FIVE_MIN : 0;
}

// contextOf is the SDK's context usage (getContextUsage) as the page shows
// it: the percent of the window in use and the tokens behind it.
export function contextOf(r) {
  return { percent: Math.round(Number(r.percentage) || 0), used: Number(r.totalTokens) || 0, max: Number(r.maxTokens) || 0 };
}

// backgroundTasks is the session's running background work as its page
// lists it (background_tasks_changed replaces the whole set each time):
// shells such as a dev server, monitors, helpers sent off. Watchers Claude
// Code marks ambient are not activity, and are left out. since is when
// this runner first saw the task's run (Claude Code sends no start time),
// kept in seen by run id (task id before CLIs that have one); a run no
// longer in the set is forgotten. A runner that restarts starts counting
// again, which is when Claude Code's own set starts again too.
export function backgroundTasks(list, seen = new Map(), now = Date.now()) {
  const live = (list || []).filter((t) => t && !t.ambient);
  const keys = new Set();
  const out = live.map((t) => {
    const key = String(t.run_id || t.task_id || "");
    keys.add(key);
    if (!seen.has(key)) seen.set(key, now);
    return { id: String(t.task_id || ""), type: String(t.task_type || ""), description: String(t.description || ""), agent: String(t.subagent_type || ""), since: seen.get(key) };
  });
  for (const key of [...seen.keys()]) if (!keys.has(key)) seen.delete(key);
  return out;
}

// usageLimit is what the page warns of from a rate_limit_event: null while
// the plan's limit is far, else how close it is and when it resets.
export function usageLimit(info) {
  if (!info || !info.status || info.status === "allowed") return null;
  return {
    status: String(info.status), type: String(info.rateLimitType || ""),
    utilization: typeof info.utilization === "number" ? info.utilization : null,
    resetsAt: typeof info.resetsAt === "number" ? info.resetsAt : null,
  };
}

// MODES are the permission modes a person may switch a running session to
// from its page; bypassPermissions and dontAsk are not offered.
export const MODES = ["default", "acceptEdits", "plan", "auto"];

// EFFORTS are the effort levels a person may set a running session to
// from its page (Claude Code's /effort); "" goes back to the model's own.
export const EFFORTS = ["", "low", "medium", "high", "xhigh", "max"];

// modelOf is one model a login may pick, as the page lists it; efforts are
// the levels Claude Code says it takes, when it says.
function modelOf(x) {
  const m = { value: String(x.value || ""), name: String(x.displayName || x.value || ""), model: String(x.resolvedModel || "") };
  if (Array.isArray(x.supportedEffortLevels)) m.efforts = x.supportedEffortLevels.map(String);
  return m;
}


// settingsEffort is the effort a session's settings give model, "" when
// none says: what "Default" means on its page, and what a Default picked
// while it runs goes back to (a cleared effortLevel would go to the
// model's own instead). Our reading of Claude Code's rules, which it does
// not publish whole: CLAUDE_CODE_EFFORT_LEVEL first, then the files in its
// order (user, project, local), the last that names one winning, and in
// them a model's own modelSettings line before the general effortLevel.
// A file that does not parse names none (advisorOf says so in the log).
export function settingsEffort(model, settingsTexts, env = {}) {
  const fromEnv = String(env.CLAUDE_CODE_EFFORT_LEVEL || "");
  if (EFFORTS.includes(fromEnv) && fromEnv) return fromEnv;
  const key = String(model || "").replace(/\[[^\]]*\]$/, "");
  let own = "", general = "";
  for (const text of settingsTexts || []) {
    let s;
    try { s = JSON.parse(text || "{}"); } catch { continue; }
    const line = s && s.modelSettings && s.modelSettings[key];
    if (line && EFFORTS.includes(line.effortLevel) && line.effortLevel) own = line.effortLevel;
    if (s && EFFORTS.includes(s.effortLevel) && s.effortLevel) general = s.effortLevel;
  }
  return own || general;
}

// advisorOf is the advisor a session runs with, as its page shows it: the
// one its row started it with (flag, --advisor), else what the settings
// files say, the last that names one winning (they come in Claude Code's
// order: user, project, local). from says which: "session", "settings",
// or "" when none names one and the session has no advisor; settings is
// what the files say either way, for the page's "Settings" option. A file
// that does not parse names none, and is told to warn by its place in the
// list; its read is the caller's to report.
export function advisorOf(flag, settingsTexts, warn = () => {}) {
  let settings = "";
  (settingsTexts || []).forEach((text, i) => {
    try {
      const s = JSON.parse(text || "{}");
      if (s && typeof s.advisorModel === "string" && s.advisorModel) settings = s.advisorModel;
    } catch (err) {
      warn(i, err);
    }
  });
  if (flag) return { model: String(flag), from: "session", settings };
  return { model: settings, from: settings ? "settings" : "", settings };
}
