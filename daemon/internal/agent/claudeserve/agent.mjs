// The sidecar's agent: one Claude Code session through the Agent SDK,
// kept as the transcript the daemon reads (translate.mjs) and driven by the
// routes (routes.mjs). The SDK and the process edges come in through the
// constructor, so agent.test.mjs runs this class against stubs without the
// SDK installed; serve.mjs is the only file that imports the real one.
import { randomUUID, randomInt } from "node:crypto";
import { Transcript, sessionInfo, permissionPattern, questionRequest, questionAnswers, questionCardText } from "./translate.mjs";
import { mcpFromFiles } from "./mcp.mjs";
import { WRITE_TOOLS, wallSafe, wallVerdict } from "./walls.mjs";
import { readOnlyVerdict } from "./readonly.mjs";
import { RUNNER_PROTOCOL, EventLog, loadState, saveState } from "./runnerstate.mjs";
import { RunnerTurns } from "./runnerturns.mjs";
import { RichTranscript, noteFor } from "./richtranscript.mjs";
import { MediaStore, mimeFor } from "./media.mjs";
import { fromInit, fromInitialization, commandList, resultOf } from "./sessionself.mjs";

// WRITE_MATCHER is the PreToolUse matcher for the file-writing tools.
const WRITE_MATCHER = Object.keys(WRITE_TOOLS).join("|");

// READ_ONLY_HOOK_SECONDS is how long a thread member's fence may take to
// answer. Its slow case is the daemon making the member's worktree (a git
// checkout, up to a minute). Past the limit Claude Code refuses the call
// itself, with its own words (seen on 2.1.326), so the fence holds either
// way; the limit only decides whose words Claude reads.
const READ_ONLY_HOOK_SECONDS = 120;

// askID is a card's id: five lowercase letters without l, the shape the
// bus's reply matchers accept ("yes abcde", "answer abcde ..."), so the
// same id names the card in the chat and on the bus.
const askAlphabet = "abcdefghijkmnopqrstuvwxyz";
export function askID() {
  let id = "";
  for (let i = 0; i < 5; i++) id += askAlphabet[randomInt(askAlphabet.length)];
  return id;
}

// readIfPresent reads a base file that may not exist yet (a hand-made base
// without mcp.json); any other failure is the start's to show.
function readIfPresent(readFile, path) {
  try {
    return readFile(path, "utf8");
  } catch (err) {
    if (err && err.code === "ENOENT") return "";
    throw err;
  }
}

export class Agent {
  // o is the parsed command line; deps carries the SDK (query, listSessions,
  // getSessionMessages) and the process edges (cwd, env, readFile, out, err,
  // fetch), so a test can hand in stubs for all of them.
  constructor(o, deps) {
    this.o = o;
    this.sdk = deps;
    this.out = deps.out;
    this.fetch = deps.fetch;
    this.cwd = deps.cwd;
    // A runner names its session with --session-auto: that id is resumed
    // when its transcript is on disk (decided in preload), opened fresh
    // when not. --session always resumes, as the agent panes expect.
    this.sid = o.session || o.sessionAuto || randomUUID();
    this.resume = !!o.session;
    // The runner's memory across its own restarts (runnerstate.mjs): only
    // a runner passes --state, so an agent pane keeps none.
    this.state = loadState(o.state, deps.readFile, (w) => this.log("! " + w));
    // A runner (--socket) tracks its turns by prompt (runnerturns.mjs); an
    // agent pane keeps the one busy flag.
    this.turns = o.socket ? new RunnerTurns(this.state) : null;
    this.events = new EventLog();
    this.claudeCode = "";
    this.boot = randomUUID(); // names this process, so the daemon sees a restart (cards it raised died with the last one)
    // Read here, not per query: an unreadable base file ends the sidecar
    // before it listens (the pane drops to its shell, read as asleep)
    // rather than failing a prompt and leaving the pane busy for good.
    this.systemPrompt = o.systemPromptFile ? deps.readFile(o.systemPromptFile, "utf8") : "";
    // The base's MCP servers, same rule: read once, fail the start when the
    // file is unreadable. A base without the file has no servers.
    // A runner with a pane starts the shim as its pane's bus owner (mcp.mjs).
    this.mcp = mcpFromFiles(o.mcpFiles.map((f) => readIfPresent(deps.readFile, f)), deps.env, o.peersCommand, !!o.socket && !!deps.env.CCMUX_PANE_ID);
    // Said at start: the SDK's init line reports only "failed" for a server
    // whose url or token came out of an unset variable.
    for (const [name, vars] of Object.entries(this.mcp.unresolved)) {
      this.log(`! mcp ${name}: ${vars.map((v) => "${" + v + "}").join(", ")} not set in this instance's environment (.env)`);
    }
    // A runner keeps the fuller transcript its page draws, with its images
    // as files in the session's media folder (richtranscript.mjs,
    // media.mjs); an agent pane keeps the chat's, as before.
    this.media = o.socket && o.media ? new MediaStore(o.media, deps.mediaFS, (w) => this.log("! " + w)) : null;
    this.transcript = this.newTranscript(this.sid);
    this.info = {}; // what the session says of itself (sessionself.mjs), for a runner's page
    this.result = null; // the last finished turn's bill
    this.clients = new Set();
    this.permissions = new Map(); // id → {request, resolve}
    this.questions = new Map();
    this.pending = ""; // text appended by tui/append-prompt, sent by tui/submit-prompt
    this.queued = new Set(); // prompt ids sent by this process
    this.feed = null; // the running query's prompt queue (see input)
    this.q = null;
    this.busy = false;
    this.turnLive = false; // a runner's turn is producing output (between its first reply and its result)
    this.interrupting = false;
    this.abort = new AbortController();
    this.daemon = deps.env.CCMUX_DAEMON_URL;
    this.pane = deps.env.CCMUX_PANE_ID;
    this.paneToken = deps.env.CCMUX_PANE_TOKEN || "";
    this.signalDown = false; // the last signal failed; said once, not per signal
    this.askDown = false; // the last card relay failed; same rule
  }

  log(line) {
    this.out(line + "\n");
  }

  newTranscript(id) {
    return this.o.socket ? new RichTranscript(id, undefined, this.media) : new Transcript(id);
  }

  // signal tells the daemon what the agent is doing (see api.agentSignal).
  // The daemon being away is not the agent's problem, so a failure never
  // stops the turn; but it is said in the terminal, once per outage, since
  // a daemon that hears nothing reads a working agent as idle and may end
  // it, and a needs-input it never hears raises no badge, flash or push
  // alert (the chat card itself rides the event stream and still shows).
  async signal(state) {
    if (!this.daemon || !this.pane) return;
    const { failure } = await this.post("agent-signal", { state });
    if (!failure) {
      this.signalDown = false;
      return;
    }
    if (!this.signalDown) this.log(`! daemon signal ${state}: ${failure} (the daemon will read this agent as idle until it answers again)`);
    this.signalDown = true;
  }

  // relayAsk hands a card to the daemon (api.agentAsk), which relays it over
  // the bus to whoever delegated to this agent; their reply comes back to the
  // card through the routes. The card is up in the chat either way, so a
  // failure only costs the relay, and is said once per outage.
  async relayAsk(body) {
    if (!this.daemon || !this.pane) return;
    const { failure, data } = await this.post("agent-ask", body);
    if (!failure) {
      this.askDown = false;
      const n = Number(data.relayed_to) || 0;
      this.log(n ? `↔ card ${body.id} relayed to ${n} peer(s) that delegated to or messaged this agent` : `↔ card ${body.id} reached no peer (no open delegation, and nobody messaged this agent in the last 10 min)`);
      return;
    }
    // A 401 is the pane's token being refused, which no amount of waiting
    // fixes; anything else is the daemon being away.
    const outlook = failure.startsWith("HTTP 401") ? "restart this agent so it gets a fresh token" : "the card is only in the chat until the daemon answers again";
    if (!this.askDown) this.log(`! card relay to the bus: ${failure} (${outlook})`);
    this.askDown = true;
  }

  // post is one loopback call to the daemon on this pane, with the pane's
  // token (the daemon's mutating routes want it). failure is "" on
  // success, else what went wrong with the daemon's reason when it gave
  // one; data is the reply body when there was a JSON one.
  async post(route, body) {
    const headers = { "content-type": "application/json" };
    if (this.paneToken) headers.authorization = `Bearer ${this.paneToken}`;
    try {
      const r = await this.fetch(`${this.daemon}/v1/panes/${this.pane}/${route}`, {
        method: "POST", headers, body: JSON.stringify(body),
      });
      const data = (await r.json?.().catch(() => null)) || {};
      if (r.ok) return { failure: "", data };
      return { failure: `HTTP ${r.status}${data.error ? ": " + data.error : ""}`, data };
    } catch (err) {
      return { failure: err && err.message ? err.message : String(err), data: {} };
    }
  }

  // broadcast numbers each event (the SSE id line) and keeps it for a
  // client that reconnects with ?after=N; readers that only want data lines
  // skip the id line.
  broadcast(ev) {
    const seq = this.events.push(ev);
    const idLine = this.o.socket ? "id: " + seq + "\n" : ""; // agent panes keep their old stream byte for byte
    const line = idLine + "data: " + JSON.stringify(ev) + "\n\n";
    for (const res of this.clients) res.write(line);
  }

  // persist writes the runner's state file; a write that fails is said and
  // the session goes on, since the state only sharpens a later recovery.
  persist() {
    if (!this.o.state) return;
    try {
      saveState(this.o.state, this.state, { writeFile: this.sdk.writeFile, rename: this.sdk.rename });
    } catch (err) {
      this.log("! state file: " + (err && err.message ? err.message : String(err)));
    }
  }

  emit(events) {
    for (const ev of events) this.broadcast(ev);
  }

  // --- the SDK query: one per process, fed prompts as they come ---

  // input is the prompt stream one query reads. Each query gets its own
  // feed (queue + waker), never a shared one: the SDK keeps pulling a dead
  // query's generator after the query ended, and a shared waker would let
  // that generator take the next prompt meant for the new query.
  async *input(feed) {
    while (!feed.dead) {
      if (feed.inbox.length === 0) await new Promise((r) => { feed.wake = r; });
      feed.wake = null;
      while (!feed.dead && feed.inbox.length) yield feed.inbox.shift();
    }
  }

  options() {
    const o = this.o;
    const opts = {
      cwd: this.cwd,
      includePartialMessages: true,
      permissionPrompts: "host",
      canUseTool: (name, input, extra) => this.canUseTool(name, input, extra),
      abortController: this.abort,
      additionalDirectories: o.addDirs,
      stderr: (data) => this.sdk.err(data),
    };
    // An agent pane asks by its base's rules; a runner is the user's own
    // session and keeps the mode their settings choose.
    if (!o.socket) opts.permissionMode = "default";
    if (this.resume) opts.resume = this.sid;
    else opts.sessionId = this.sid;
    if (o.pluginDirs.length) opts.plugins = o.pluginDirs.map((path) => ({ type: "local", path }));
    // An isolated agent: only the settings named (the home's project ones),
    // and the skills listed when some are switched off.
    // The claude.ai connectors (Gmail, Drive...) ride on the login, not on
    // any settings file, so they are switched off here too.
    if (o.settingSources) {
      opts.settingSources = o.settingSources;
      // A runner loading the user's own settings is the user's session and
      // keeps their connectors; an isolated agent (project only) does not.
      if (!o.settingSources.includes("user")) opts.env = { ...this.sdk.env, ENABLE_CLAUDEAI_MCP_SERVERS: "false" };
    }
    if (o.claude) opts.pathToClaudeCodeExecutable = o.claude;
    if (o.skills) opts.skills = o.skills;
    if (this.systemPrompt) opts.systemPrompt = { type: "preset", preset: "claude_code", append: this.systemPrompt };
    if (o.model) opts.model = o.model;
    if (Object.keys(this.mcp.servers).length) opts.mcpServers = this.mcp.servers;
    // The base's gates, then every MCP server's tools and the peers shim's:
    // the base chose those servers, so a card for their tools asks nothing.
    opts.allowedTools = o.allowed.concat(this.mcp.allow);
    if (o.disallowed.length) opts.disallowedTools = o.disallowed;
    const before = this.preToolHooks();
    if (before.length) opts.hooks = { PreToolUse: before };
    return opts;
  }

  // preToolHooks are the hard stops that run before any tool: the walls of
  // a walled agent (its write tools), and the fences of a thread member
  // (every tool, since a shell command writes too).
  preToolHooks() {
    const o = this.o;
    const hooks = [];
    if (o.writeRoots && o.writeRoots.length) hooks.push({ matcher: WRITE_MATCHER, hooks: [(input) => this.wallHook(input)] });
    if (o.readOnly || o.keepOut) hooks.push({ hooks: [(input) => this.readOnlyHook(input)], timeout: READ_ONLY_HOOK_SECONDS });
    return hooks;
  }

  // readOnlyHook is a thread member's fence (readonly.mjs). It never lets a
  // call through by accident: Claude Code runs the tool when a hook throws
  // (seen on Claude Code 2.1.326), so anything that goes wrong here, the
  // check itself included, is a refusal.
  async readOnlyHook(input) {
    let reason;
    try {
      const verdict = readOnlyVerdict(input.tool_name, input.tool_input, this.o, this.cwd);
      if (!verdict) return {};
      reason = verdict.move ? await this.askForWorktree(input.tool_name) : verdict.deny;
    } catch (err) {
      reason = `ccmux: this call could not be checked against the session's read-only rule (${err && err.message ? err.message : String(err)}), so it is refused.`;
    }
    this.log(`read only: ${input.tool_name} refused: ${reason}`);
    return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: reason } };
  }

  // askForWorktree asks the daemon to move this read-only member into its
  // own worktree, and returns what the refused call is answered with. The
  // daemon restarts this runner in the worktree a moment after it answers;
  // what it wants said there is kept for that start (RunnerState.note).
  // Without a daemon, or when it could not make the worktree, the call is
  // refused all the same and the answer says why.
  async askForWorktree(tool) {
    const { failure, data } = await this.post("first-edit", { tool });
    if (failure) {
      return `ccmux: this session only reads in ${this.cwd} until it has its own worktree, and the worktree could not be made (${failure}). Nothing was changed. Tell your lead, or the person, what you need.`;
    }
    if (data.notice) {
      this.state.note = String(data.notice);
      this.persist();
    }
    return String(data.message || "ccmux: this session is being moved into its own worktree and restarts there in a moment. Make this change again after the restart.");
  }

  // wallHook is the walls' hard stop. A PreToolUse hook runs before every
  // permission rule, so an allow rule saved in a settings file the SDK loads
  // (an old "always" answer in the home's .claude/settings.local.json) cannot
  // approve a write outside the roots. Inside them it decides nothing and
  // the write goes on to the rules and canUseTool as before.
  async wallHook(input) {
    const wall = wallVerdict(input.tool_name, input.tool_input, this.o, this.cwd);
    if (!wall || !wall.deny) return {};
    this.log(`walls: ${input.tool_name} refused: ${wall.deny}`);
    return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: wall.deny } };
  }

  // ensureQuery has the SDK session running; a session that ended (an
  // error) is started again by the next prompt, continuing the conversation
  // when a reply of it is on disk (resume) and under a fresh id otherwise:
  // Claude refuses a fresh start under an id that already exists on disk,
  // and a resume of one that was never written.
  ensureQuery() {
    if (this.q) return;
    this.feed = { inbox: [], wake: null, dead: false };
    this.q = this.sdk.query({ prompt: this.input(this.feed), options: this.options() });
    if (this.turns) this.askSelf(this.q);
    this.pump().catch((err) => this.log("! " + (err && err.message ? err.message : String(err))));
  }

  // pump reads the SDK stream until it ends; the next prompt starts a new
  // query (see ensureQuery for which conversation it continues).
  async pump() {
    const q = this.q;
    const feed = this.feed;
    try {
      for await (const m of q) this.onMessage(m);
    } catch (err) {
      if (!this.abort.signal.aborted) {
        const text = err && err.message ? err.message : String(err);
        this.log("! " + text);
        // The failure lands on the reply that was being written, when there
        // is one; otherwise (died at boot, died before replying, died while
        // idle) on a turn of its own, never on a reply that finished.
        const mid = this.busy && this.transcript.currentAssistant();
        this.emit(mid ? this.transcript.finishTurn(text) : this.transcript.errorTurn(text));
        this.broadcast({ type: "session.error", properties: { sessionID: this.sid, error: text } });
        this.broadcast({ type: "session.idle", properties: { sessionID: this.sid } });
      }
    } finally {
      if (this.q === q) this.q = null;
      // This query's feed dies with it (its generator returns, and holds no
      // shared state), and an interrupt raised on this query must not hide
      // the next one's error.
      feed.dead = true;
      if (feed.wake) feed.wake();
      // A runner whose Claude died ends with it: systemd starts it again,
      // and the start is the one path that resumes, sends the notice and
      // counts toward the restart cap. It keeps its session id throughout.
      if (this.turns && !this.abort.signal.aborted) {
        this.log("! Claude stopped; ending this runner so it starts again and resumes");
        this.sdk.exit(1);
        return;
      }
      if (this.feed === feed) this.feed = null;
      this.interrupting = false;
      if (!this.abort.signal.aborted && !this.resume) this.rotateSession();
      this.setBusy(false);
    }
  }

  // rotateSession gives the next start a fresh conversation id: nothing of
  // this one reached disk worth continuing, and the id itself may already be
  // taken there. The chat is told (session.created), and follows.
  rotateSession() {
    this.sid = randomUUID();
    this.transcript.sessionID = this.sid;
    this.log(`⚙ next start opens a new conversation (${this.sid.slice(0, 8)})`);
    this.broadcast({ type: "session.created", properties: { info: { id: this.sid, directory: this.cwd } } });
  }

  onMessage(m) {
    if (m.type === "assistant") this.resume = true; // a reply is on disk: this conversation can be continued
    this.noteDelivered(m);
    if (m.type === "command_lifecycle") {
      this.onLifecycle(m);
      return;
    }
    if (m.type === "system") {
      this.onSystem(m);
      return;
    }
    if (m.type === "result") {
      this.turnLive = false;
      this.onResult(m);
      return;
    }
    // A runner's turn that no prompt of ours opened (a background hook woke
    // the session with its findings) is still a turn: busy until its result,
    // so its page shows it working and Stop reaches it.
    if (this.turns && (m.type === "assistant" || m.type === "stream_event")) {
      this.turnLive = true;
      this.setBusy(true);
    }
    const events = this.transcript.apply(m);
    this.emit(events);
    this.print(m);
  }

  // onLifecycle is Claude Code saying where one prompt stands: started (it
  // has it) and completed (it is done with it). A runner counts on this for
  // a prompt that arrived while a turn was running: Claude Code folds that
  // one into the running turn, and nothing else ever names it, neither a
  // reply nor a line of the transcript. Without it the daemon would hold
  // the prompt as undelivered for good, and send it again, to run a second
  // time, after the next restart.
  onLifecycle(m) {
    if (!this.turns || !m.command_uuid) return;
    const done = m.state === "completed";
    let changed = done || m.state === "started" ? this.state.markDelivered([m.command_uuid]) : false;
    if (done) changed = this.turns.completed(m.command_uuid) || changed;
    if (changed) this.persist();
    // Said after the turn's result (its usual place): nothing further will
    // settle the busy flag, so this does.
    if (done && !this.turnLive) this.setBusy(this.turns.busy());
  }

  // onSystem is Claude Code speaking beside the conversation. The terminal
  // gets the line that starts a turn; a runner's page gets what the session
  // says of itself, and the notes worth showing where they happened.
  onSystem(m) {
    if (m.subtype === "init") {
      this.claudeCode = m.claude_code_version || this.claudeCode;
      const mcp = (m.mcp_servers || []).map((x) => `${x.name} ${x.status}`).join(", ");
      this.log(`⚙ ${this.o.agent} on ${m.model} (session ${this.sid.slice(0, 8)})${mcp ? " · mcp: " + mcp : ""}`);
    }
    if (!this.turns) return;
    if (m.subtype === "init") this.learn(fromInit(m, this.info));
    else if (m.subtype === "commands_changed") this.learn({ commands: commandList(m.commands) });
    else this.emit(this.transcript.note(noteFor(m), m.level));
  }

  // learn takes in something the session said of itself and tells the page
  // when it is news: the init line comes with every turn, and mostly says
  // what it said the last time.
  learn(self) {
    const next = { ...this.info, ...self };
    if (JSON.stringify(next) === JSON.stringify(this.info)) return;
    this.info = next;
    this.broadcast({ type: "session.info", properties: { sessionID: this.sid, info: this.info } });
  }

  // askSelf asks a new query what it offers (its commands, the models to
  // pick from), which it knows before any turn. A query that cannot say
  // (it died first) costs the page those lists until the first turn.
  askSelf(q) {
    if (typeof q.initializationResult !== "function") return;
    q.initializationResult()
      .then((r) => { if (this.q === q) this.learn(fromInitialization(r)); })
      .catch((err) => this.log("! what this session offers was not read: " + (err && err.message ? err.message : String(err))));
  }

  // noteDelivered records the prompts Claude has: a reply names the prompt
  // it answers (user_message_uuid), and the SDK echoes a user message back
  // with its id.
  noteDelivered(m) {
    const ids = [];
    if (m.type === "assistant") ids.push(m.user_message_uuid, ...(m.user_message_uuids || []));
    if (m.type === "user" && !m.parent_tool_use_id) ids.push(m.uuid);
    if (this.turns && m.type === "assistant") this.turns.stamped(ids);
    if (this.state.markDelivered(ids.filter(Boolean))) this.persist();
  }

  onResult(m) {
    if (this.turns && this.turns.result()) this.persist();
    const failed = m.is_error && !this.interrupting;
    this.interrupting = false;
    if (failed) {
      const text = m.result || m.subtype || "error";
      this.emit(this.transcript.finishTurn(text));
      this.broadcast({ type: "session.error", properties: { sessionID: this.sid, error: text } });
      this.log("! " + text);
    }
    const idle = { sessionID: this.sid };
    if (this.turns) idle.result = this.result = resultOf(m); // what the turn cost, for the session's page
    this.broadcast({ type: "session.idle", properties: idle });
    this.setBusy(this.turns ? this.turns.busy() : false);
  }

  // print is the terminal view: reply text as it streams, tool calls on
  // their own lines.
  print(m) {
    if (m.type === "stream_event") {
      const d = m.event.delta;
      if (m.event.type === "content_block_delta" && d && d.type === "text_delta") this.out(d.text);
      if (m.event.type === "message_stop") this.out("\n");
      return;
    }
    if (m.type === "assistant") {
      for (const b of m.message.content || []) {
        if (b.type === "tool_use") this.log(`⚙ ${b.name}: ${permissionPattern(b.name, b.input)}`);
      }
    }
  }

  setBusy(v) {
    if (this.busy === v) return;
    this.busy = v;
    this.signal(v ? "busy" : "idle");
  }

  // --- what the daemon and the terminal send in ---

  // prompt sends text as the next turn. id is the prompt's own uuid, which
  // lands in the transcript and on the reply; a runner passes the inbox id,
  // and a prompt whose id Claude already has is dropped, so a resend after a
  // crash never runs twice. It reports whether the prompt was sent.
  //
  // images are the names of pictures that go with a runner's prompt, stored
  // in its media folder (by the daemon, at upload): Claude gets them as
  // image blocks before the text, the transcript as file parts.
  prompt(text, id = "", images = []) {
    if (id && (this.state.isDelivered(id) || this.queued.has(id))) return false;
    const uuid = id || randomUUID();
    this.queued.add(uuid);
    const { content, files } = this.promptContent(text, images);
    this.emit(this.transcript.userTurn(text, uuid, files));
    this.log("> " + text + (files.length ? ` [+${files.length} image${files.length === 1 ? "" : "s"}]` : ""));
    if (this.turns) {
      this.turns.sent(uuid, text);
      this.persist();
    }
    this.setBusy(true);
    this.ensureQuery();
    this.feed.inbox.push({ type: "user", uuid, message: { role: "user", content }, parent_tool_use_id: null });
    if (this.feed.wake) this.feed.wake();
    return true;
  }

  // promptContent is what Claude is sent for a prompt: the text alone, or
  // its pictures as image blocks and then the text. A picture that cannot
  // be read (its file is gone, or this session keeps none) never holds the
  // prompt back: the inbox is delivered in order, so a prompt that could not
  // go out would hold every later one with it. It goes without the picture,
  // and says so to Claude and in the log.
  promptContent(text, images) {
    const blocks = [];
    const files = [];
    for (const name of images) {
      try {
        if (!this.media) throw new Error("this session keeps no images");
        blocks.push(this.media.block(name));
        files.push({ mime: mimeFor(name), name });
      } catch (err) {
        this.log(`! image ${name} not attached: ${err && err.message ? err.message : String(err)}`);
      }
    }
    const lost = images.length - blocks.length;
    const said = lost ? `${text}\n[ccmux: ${lost} image${lost === 1 ? "" : "s"} attached to this message could not be read, so ${lost === 1 ? "it is" : "they are"} missing here.]`.trim() : text;
    if (!blocks.length) return { content: said, files };
    return { content: said ? [...blocks, { type: "text", text: said }] : blocks, files };
  }


  // hello is what a runner says at connect: versions to check, and where
  // the session is.
  hello() {
    return {
      protocol: RUNNER_PROTOCOL, boot: this.boot, session: this.sid, busy: this.busy, turnOpen: !!this.turns && this.turns.busy(),
      seq: this.events.seq, model: this.o.model || "", claudeCode: this.claudeCode, sdk: this.sdk.sdkVersion || "",
    };
  }

  // snapshot is the conversation as it stands, with the number of the last
  // event already in it: a reader shows this, then follows the events after
  // seq, and has missed nothing and seen nothing twice. Everything here is
  // read in one go, with no await between the number and the conversation.
  snapshot() {
    return {
      protocol: RUNNER_PROTOCOL, boot: this.boot, seq: this.events.seq, session: this.sid, busy: this.busy,
      messages: this.transcript.messages(), permissions: this.pendingPermissions(), questions: this.pendingQuestions(),
      info: this.info, result: this.result, todos: this.transcript.todos || [], sdk: this.sdk.sdkVersion || "",
    };
  }

  // delivered is the subset of ids Claude has.
  delivered(ids) {
    return (ids || []).filter((id) => this.state.isDelivered(id));
  }

  // interrupt stops the running turn: pending cards are answered as denied
  // so the SDK's promises settle, then the SDK is told to stop. A Stop that
  // lands after the turn already ended does nothing; the flag it sets is
  // only ever read by the result of the turn it was raised on.
  async interrupt() {
    if (!this.q || !this.busy) return;
    this.interrupting = true;
    for (const [id] of this.permissions) this.replyPermission(id, "reject");
    for (const [id] of this.questions) this.rejectQuestion(id);
    try {
      await this.q.interrupt();
    } catch (err) {
      this.interrupting = false;
      throw err;
    }
  }

  // canUseTool is the SDK asking before a tool runs: a permission card in
  // the chat, or a question card for AskUserQuestion, answered by a human.
  canUseTool(name, input, extra) {
    if (name === "AskUserQuestion") return this.askQuestion(input, extra);
    const wall = wallVerdict(name, input, this.o, this.cwd);
    if (wall === "allow") return Promise.resolve({ behavior: "allow", updatedInput: input });
    if (wall && wall.deny) {
      this.log(`walls: ${name} refused: ${wall.deny}`);
      return Promise.resolve({ behavior: "deny", message: wall.deny });
    }
    const id = askID();
    // The SDK's suggestions are the rule "always" would write; when it says
    // that rule would grant more than this one ask, "always" is not offered.
    // Nor on a walled write: its suggestion switches to acceptEdits, which
    // approves every edit in the added folders without asking canUseTool.
    // And on any walled agent's card, never one that would lift the walls.
    let suggestions = extra.suppressAlwaysAllowRule || wall === "ask" ? [] : extra.suggestions || [];
    if (this.o.writeRoots && this.o.writeRoots.length) suggestions = wallSafe(suggestions);
    // An isolated start reads no local settings, so an "always" saved there
    // would be forgotten at the next wake: it goes to the home's project
    // settings, which that start does read.
    if (this.o.settingSources && !this.o.settingSources.includes("user")) suggestions = suggestions.map((sg) => (sg.destination === "localSettings" ? { ...sg, destination: "projectSettings" } : sg));
    const request = {
      id, sessionID: this.sid, permission: name, patterns: [permissionPattern(name, input)],
      always: suggestions.length ? ["*"] : [],
      metadata: { input, title: extra.title || "", description: extra.description || "" },
      tool: { messageID: "", callID: extra.toolUseID || "" },
    };
    this.log(`? ${name}: ${request.patterns[0]}`);
    return new Promise((resolve) => {
      this.permissions.set(id, { request, resolve, suggestions });
      this.broadcast({ type: "permission.asked", properties: request });
      this.signal("permission");
      this.relayAsk({ kind: "permission", id, tool: name, description: extra.description || extra.title || request.patterns[0], preview: request.patterns[0] });
      extra.signal?.addEventListener("abort", () => this.replyPermission(id, "reject"), { once: true });
    });
  }

  pendingPermissions() {
    return [...this.permissions.values()].map((p) => p.request);
  }

  pendingQuestions() {
    return [...this.questions.values()].map((q) => q.request);
  }

  replyPermission(id, reply) {
    const p = this.permissions.get(id);
    if (!p) return false;
    this.permissions.delete(id);
    // once and always allow (always also writes the SDK's suggested rule when
    // there is one); reject, and anything this code does not know, denies.
    if (reply === "once" || reply === "always") {
      const allow = { behavior: "allow" };
      if (reply === "always" && p.suggestions.length) allow.updatedPermissions = p.suggestions;
      p.resolve(allow);
    } else {
      p.resolve({ behavior: "deny", message: "Denied by the human in the ccmux chat." });
    }
    this.broadcast({ type: "permission.replied", properties: { sessionID: this.sid, requestID: id, reply } });
    this.signal("replied");
    return true;
  }

  askQuestion(input, extra) {
    const id = askID();
    const request = questionRequest(id, this.sid, input, extra.toolUseID);
    this.log(`? ${request.questions.map((q) => q.question).join(" / ")}`);
    return new Promise((resolve) => {
      this.questions.set(id, { request, resolve, input });
      this.broadcast({ type: "question.asked", properties: request });
      this.signal("question");
      this.relayAsk({ kind: "question", id, text: questionCardText(request) });
      extra.signal?.addEventListener("abort", () => this.rejectQuestion(id), { once: true });
    });
  }

  replyQuestion(id, answers) {
    const q = this.questions.get(id);
    if (!q) return false;
    this.questions.delete(id);
    q.resolve({ behavior: "allow", updatedInput: { ...q.input, answers: questionAnswers(q.input, answers) } });
    this.broadcast({ type: "question.replied", properties: { sessionID: this.sid, requestID: id } });
    this.signal("replied");
    return true;
  }

  rejectQuestion(id) {
    const q = this.questions.get(id);
    if (!q) return false;
    this.questions.delete(id);
    q.resolve({ behavior: "deny", message: "The human declined to answer." });
    this.broadcast({ type: "question.rejected", properties: { sessionID: this.sid, requestID: id } });
    this.signal("replied");
    return true;
  }

  // --- history ---

  // sessions is the conversations on disk plus the current one before its
  // first turn is written. A store that cannot be read throws: the route
  // answers 500 and the daemon says so in the chat, the same as the asleep
  // read would, rather than passing off a partial list as the history.
  async sessions() {
    const list = (await this.sdk.listSessions({ dir: this.cwd })).map(sessionInfo);
    if (!list.some((s) => s.id === this.sid)) {
      list.unshift({ id: this.sid, title: "", directory: this.cwd, time: { created: Date.now(), updated: Date.now() } });
    }
    return list;
  }

  async messages(id) {
    if (id === this.sid) return this.transcript.messages();
    const msgs = await this.sdk.getSessionMessages(id, { dir: this.cwd });
    const t = this.newTranscript(id);
    for (const m of msgs) t.apply(m);
    return t.messages();
  }

  // preload fills the transcript from disk when resuming, so the chat's
  // hello shows the earlier conversation before the first new prompt. A
  // conversation that cannot be read ends the start (main exits, the pane
  // drops to its shell) instead of continuing it as an empty transcript.
  async preload() {
    if (!this.resume && !this.o.sessionAuto) return;
    const msgs = await this.sdk.getSessionMessages(this.sid, { dir: this.cwd });
    if (this.o.sessionAuto) this.resume = msgs.length > 0;
    for (const m of msgs) {
      this.transcript.apply(m);
      if (m.type === "user" && m.uuid) this.state.markDelivered([m.uuid]);
    }
  }

  // recover runs once at a runner's start, after preload: a turn that was
  // cut off gets the interruption notice as the next prompt.
  recover() {
    if (!this.turns) return;
    const notice = this.turns.notice();
    if (!notice) return;
    this.persist();
    this.prompt(notice);
  }

  // shutdown ends the SDK session and the event streams; the caller exits.
  // An agent pane says idle as it goes: its lifecycle clock reads that. A
  // runner does not: to the daemon idle is a finished turn, which flashes
  // and, a minute on, alerts, and a session put to sleep or restarted has
  // finished nothing. One stopped between turns has nothing new to say
  // (what it last said stands); one stopped mid-turn says so, since that
  // turn, and any card it had up, is gone.
  async shutdown() {
    this.log("⚙ ending the session");
    this.abort.abort();
    for (const res of this.clients) res.end();
    if (!this.turns) await this.signal("idle");
    else if (this.busy) await this.signal("stopped");
  }
}
