// The sidecar's agent: one Claude Code session through the Agent SDK,
// kept as the transcript the daemon reads (translate.mjs) and driven by the
// routes (routes.mjs). The SDK and the process edges come in through the
// constructor, so agent.test.mjs runs this class against stubs without the
// SDK installed; serve.mjs is the only file that imports the real one.
import { randomUUID } from "node:crypto";
import { Transcript, sessionInfo, permissionPattern, questionRequest, questionAnswers } from "./translate.mjs";

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
    this.sid = o.session || randomUUID();
    this.resume = !!o.session;
    // Read here, not per query: an unreadable base file ends the sidecar
    // before it listens (the pane drops to its shell, read as asleep)
    // rather than failing a prompt and leaving the pane busy for good.
    this.systemPrompt = o.systemPromptFile ? deps.readFile(o.systemPromptFile, "utf8") : "";
    this.transcript = new Transcript(this.sid);
    this.clients = new Set();
    this.permissions = new Map(); // id → {request, resolve}
    this.questions = new Map();
    this.pending = ""; // text appended by tui/append-prompt, sent by tui/submit-prompt
    this.feed = null; // the running query's prompt queue (see input)
    this.q = null;
    this.busy = false;
    this.interrupting = false;
    this.abort = new AbortController();
    this.reqSeq = 0;
    this.daemon = deps.env.CCMUX_DAEMON_URL;
    this.pane = deps.env.CCMUX_PANE_ID;
    this.signalDown = false; // the last signal failed; said once, not per signal
  }

  log(line) {
    this.out(line + "\n");
  }

  // signal tells the daemon what the agent is doing (see api.agentSignal).
  // The daemon being away is not the agent's problem, so a failure never
  // stops the turn; but it is said in the terminal, once per outage, since
  // a daemon that hears nothing reads a working agent as idle and may end
  // it, and a needs-input it never hears raises no badge, flash or push
  // alert (the chat card itself rides the event stream and still shows).
  async signal(state) {
    if (!this.daemon || !this.pane) return;
    let failure = "";
    try {
      const r = await this.fetch(`${this.daemon}/v1/panes/${this.pane}/agent-signal`, {
        method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ state }),
      });
      if (!r.ok) failure = `HTTP ${r.status}`;
    } catch (err) {
      failure = err && err.message ? err.message : String(err);
    }
    if (!failure) {
      this.signalDown = false;
      return;
    }
    if (!this.signalDown) this.log(`! daemon signal ${state}: ${failure} (the daemon will read this agent as idle until it answers again)`);
    this.signalDown = true;
  }

  broadcast(ev) {
    const line = "data: " + JSON.stringify(ev) + "\n\n";
    for (const res of this.clients) res.write(line);
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
      permissionMode: "default",
      permissionPrompts: "host",
      canUseTool: (name, input, extra) => this.canUseTool(name, input, extra),
      abortController: this.abort,
      additionalDirectories: o.addDirs,
      stderr: (data) => this.sdk.err(data),
    };
    if (this.resume) opts.resume = this.sid;
    else opts.sessionId = this.sid;
    if (o.pluginDir) opts.plugins = [{ type: "local", path: o.pluginDir }];
    if (this.systemPrompt) opts.systemPrompt = { type: "preset", preset: "claude_code", append: this.systemPrompt };
    if (o.model) opts.model = o.model;
    if (o.allowed.length) opts.allowedTools = o.allowed;
    if (o.disallowed.length) opts.disallowedTools = o.disallowed;
    return opts;
  }

  ensureQuery() {
    if (this.q) return;
    this.feed = { inbox: [], wake: null, dead: false };
    this.q = this.sdk.query({ prompt: this.input(this.feed), options: this.options() });
    this.pump().catch((err) => this.log("! " + (err && err.message ? err.message : String(err))));
  }

  // pump reads the SDK stream until it ends; the next prompt starts a new
  // query that resumes the same session.
  async pump() {
    const q = this.q;
    const feed = this.feed;
    try {
      for await (const m of q) this.onMessage(m);
    } catch (err) {
      if (!this.abort.signal.aborted) {
        const text = err && err.message ? err.message : String(err);
        this.log("! " + text);
        this.emit(this.transcript.finishTurn(text));
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
      if (this.feed === feed) this.feed = null;
      this.interrupting = false;
      this.resume = true;
      this.setBusy(false);
    }
  }

  onMessage(m) {
    if (m.type === "system" && m.subtype === "init") {
      const mcp = (m.mcp_servers || []).map((x) => `${x.name} ${x.status}`).join(", ");
      this.log(`⚙ ${this.o.agent} on ${m.model} (session ${this.sid.slice(0, 8)})${mcp ? " · mcp: " + mcp : ""}`);
      return;
    }
    if (m.type === "result") {
      this.onResult(m);
      return;
    }
    const events = this.transcript.apply(m);
    this.emit(events);
    this.print(m);
  }

  onResult(m) {
    const failed = m.is_error && !this.interrupting;
    this.interrupting = false;
    if (failed) {
      const text = m.result || m.subtype || "error";
      this.emit(this.transcript.finishTurn(text));
      this.broadcast({ type: "session.error", properties: { sessionID: this.sid, error: text } });
      this.log("! " + text);
    }
    this.broadcast({ type: "session.idle", properties: { sessionID: this.sid } });
    this.setBusy(false);
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

  prompt(text) {
    this.emit(this.transcript.userTurn(text));
    this.log("> " + text);
    this.setBusy(true);
    this.ensureQuery();
    this.feed.inbox.push({ type: "user", message: { role: "user", content: text }, parent_tool_use_id: null });
    if (this.feed.wake) this.feed.wake();
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
    const id = "per_" + ++this.reqSeq;
    // The SDK's suggestions are the rule "always" would write; when it says
    // that rule would grant more than this one ask, "always" is not offered.
    const suggestions = extra.suppressAlwaysAllowRule ? [] : extra.suggestions || [];
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
      this.signal("needs-input");
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
    const id = "que_" + ++this.reqSeq;
    const request = questionRequest(id, this.sid, input, extra.toolUseID);
    this.log(`? ${request.questions.map((q) => q.question).join(" / ")}`);
    return new Promise((resolve) => {
      this.questions.set(id, { request, resolve, input });
      this.broadcast({ type: "question.asked", properties: request });
      this.signal("needs-input");
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
    const t = new Transcript(id);
    for (const m of msgs) t.apply(m);
    return t.messages();
  }

  // preload fills the transcript from disk when resuming, so the chat's
  // hello shows the earlier conversation before the first new prompt. A
  // conversation that cannot be read ends the start (main exits, the pane
  // drops to its shell) instead of continuing it as an empty transcript.
  async preload() {
    if (!this.resume) return;
    for (const m of await this.sdk.getSessionMessages(this.sid, { dir: this.cwd })) this.transcript.apply(m);
  }

  // shutdown ends the SDK session and the event streams; the caller exits.
  async shutdown() {
    this.log("⚙ ending the session");
    this.abort.abort();
    for (const res of this.clients) res.end();
    await this.signal("idle");
  }
}
