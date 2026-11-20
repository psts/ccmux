// node --test agent.test.mjs — the agent class against a stub SDK: what a
// permission or question reply resolves to, what an interrupt does, which
// signals the daemon gets, and how a failed query ends.
import { test } from "node:test";
import assert from "node:assert/strict";
import { parseArgs } from "./args.mjs";
import { Agent, askID } from "./agent.mjs";

// stubSDK records what the agent asks of it. query() returns an object the
// agent iterates: it yields the scripted messages, then waits until the test
// ends it (or throws when told to).
function stubSDK(script = []) {
  const sdk = {
    queries: [], sessions: [], stored: {}, err: [], listFails: false,
    // Like the SDK, a query pulls the prompt iterable as soon as it exists;
    // what it pulled is in q.sent.
    query: ({ prompt, options }) => {
      const q = { prompt, options, sent: [], interrupts: 0, interruptFails: false, ended: null };
      (async () => { for await (const m of prompt) q.sent.push(m); })();
      q.interrupt = async () => { q.interrupts++; if (q.interruptFails) throw new Error("no turn to interrupt"); };
      q[Symbol.asyncIterator] = async function* () {
        for (const m of script) {
          if (m instanceof Error) throw m;
          yield m;
        }
        await new Promise((r) => { q.ended = r; });
      };
      sdk.queries.push(q);
      return q;
    },
    listSessions: async () => { if (sdk.listFails) throw new Error("store locked"); return sdk.sessions; },
    getSessionMessages: async (id) => sdk.stored[id] || [],
  };
  return sdk;
}

function makeAgent(opts = {}, script = []) {
  const sdk = stubSDK(script);
  const out = [];
  const signals = [];
  const asks = []; // what reached POST .../agent-ask
  const headers = []; // the headers of every daemon call
  const firstEdits = []; // what reached POST .../first-edit
  const deps = {
    ...sdk, cwd: "/inst", env: { CCMUX_DAEMON_URL: "http://d", CCMUX_PANE_ID: "p1", CCMUX_PANE_TOKEN: "tok-p1", ...(opts.env || {}) },
    readFile: opts.readFile || (() => "# base\n"), out: (s) => out.push(s), err: (s) => sdk.err.push(s),
    fetch: async (url, init) => {
      headers.push(init.headers);
      if (url.endsWith("/agent-ask")) {
        asks.push(JSON.parse(init.body));
        if (opts.askStatus) return { ok: false, status: opts.askStatus, json: async () => ({ error: opts.askStatus === 401 ? "invalid pane token" : "no peer is registered on this pane" }) };
        return { ok: true, json: async () => ({ ok: true, relayed_to: opts.relayedTo ?? 1 }) };
      }
      if (url.endsWith("/first-edit")) {
        firstEdits.push(JSON.parse(init.body));
        return opts.firstEdit();
      }
      assert.equal(url, "http://d/v1/panes/p1/agent-signal");
      signals.push(JSON.parse(init.body).state);
      if (opts.signalFails) throw new Error("connect ECONNREFUSED");
      if (opts.signalStatus) return { ok: false, status: opts.signalStatus };
      return { ok: true };
    },
  };
  const agent = new Agent({ agent: "probe", addDirs: [], allowed: [], disallowed: [], pluginDirs: [], mcpFiles: [], ...opts }, deps);
  const events = [];
  agent.clients.add({ write: (line) => events.push(JSON.parse(line.slice(6))), end() {} });
  return { agent, sdk, out, signals, asks, headers, events, firstEdits };
}

const tick = () => new Promise((r) => setTimeout(r, 5));

test("permission replies: once allows, always writes the suggested rule, reject and anything unknown deny", async () => {
  const { agent, events } = makeAgent();
  const S = { type: "setMode", mode: "acceptEdits", destination: "session" };
  const pOnce = agent.canUseTool("Bash", { command: "ls" }, { toolUseID: "c1", suggestions: [S] });
  const pAlways = agent.canUseTool("Write", { file_path: "/x" }, { toolUseID: "c2", suggestions: [S] });
  const pReject = agent.canUseTool("Bash", { command: "rm -rf /" }, { toolUseID: "c3" });
  const pOdd = agent.canUseTool("Bash", { command: "cat" }, { toolUseID: "c4" });
  const pNoRule = agent.canUseTool("Bash", { command: "pwd" }, { toolUseID: "c5" });
  const ids = agent.pendingPermissions().map((r) => r.id);
  assert.equal(ids.length, 5);
  assert.deepEqual(agent.pendingPermissions()[0].patterns, ["ls"]);
  assert.deepEqual(agent.pendingPermissions()[0].always, ["*"], "a suggestion means always can be offered");
  assert.deepEqual(agent.pendingPermissions()[2].always, [], "no suggestion, no always");
  assert.equal(agent.replyPermission(ids[0], "once"), true);
  assert.equal(agent.replyPermission(ids[1], "always"), true);
  assert.equal(agent.replyPermission(ids[2], "reject"), true);
  assert.equal(agent.replyPermission(ids[3], "deny"), true, "an unknown reply is still answered");
  assert.equal(agent.replyPermission(ids[4], "always"), true);
  assert.equal(agent.replyPermission("per_99", "once"), false);
  assert.deepEqual(await pOnce, { behavior: "allow" });
  assert.deepEqual(await pAlways, { behavior: "allow", updatedPermissions: [S] });
  assert.equal((await pReject).behavior, "deny");
  assert.equal((await pOdd).behavior, "deny", "only once and always may run a tool");
  assert.deepEqual(await pNoRule, { behavior: "allow" }, "always with nothing to persist is a plain allow");
  assert.equal(agent.pendingPermissions().length, 0);
  assert.deepEqual(events.filter((e) => e.type === "permission.asked").length, 5);
  assert.deepEqual(events.filter((e) => e.type === "permission.replied").map((e) => e.properties.reply), ["once", "always", "reject", "deny", "always"]);
});

test("the SDK's no-persistent-rule flag hides always and never writes the rule", async () => {
  const { agent } = makeAgent();
  const S = { type: "addRules", rules: [{ toolName: "Bash" }], behavior: "allow", destination: "session" };
  const p = agent.canUseTool("Bash", { command: "curl x" }, { toolUseID: "c1", suggestions: [S], suppressAlwaysAllowRule: true });
  const [req] = agent.pendingPermissions();
  assert.deepEqual(req.always, []);
  agent.replyPermission(req.id, "always");
  assert.deepEqual(await p, { behavior: "allow" });
});

test("questions: an answer goes back as the tool's answers map, a rejection denies", async () => {
  const { agent, events } = makeAgent();
  const input = { questions: [{ question: "Tea or coffee?", header: "Drink", options: [{ label: "Tea" }, { label: "Coffee" }], multiSelect: false }] };
  const p = agent.canUseTool("AskUserQuestion", input, { toolUseID: "c1" });
  const [req] = agent.pendingQuestions();
  assert.equal(req.questions[0].header, "Drink");
  assert.equal(agent.replyQuestion(req.id, [["Coffee"]]), true);
  const r = await p;
  assert.equal(r.behavior, "allow");
  assert.deepEqual(r.updatedInput.answers, { "Tea or coffee?": "Coffee" });
  const p2 = agent.canUseTool("AskUserQuestion", input, { toolUseID: "c2" });
  assert.equal(agent.rejectQuestion(agent.pendingQuestions()[0].id), true);
  assert.equal((await p2).behavior, "deny");
  assert.equal(agent.rejectQuestion("que_99"), false);
  assert.deepEqual(events.map((e) => e.type), ["question.asked", "question.replied", "question.asked", "question.rejected"]);
});

test("the session can be started before any prompt, idle, and the first prompt then feeds it", async () => {
  const { agent, sdk, signals } = makeAgent();
  agent.ensureQuery();
  await tick();
  assert.equal(sdk.queries.length, 1, "the Claude child exists before a prompt");
  assert.deepEqual(sdk.queries[0].sent, [], "nothing was sent");
  assert.equal(agent.busy, false);
  assert.deepEqual(signals, [], "an idle start signals nothing");
  agent.prompt("hello");
  await tick();
  assert.equal(sdk.queries.length, 1, "the same session takes the prompt");
  assert.deepEqual(sdk.queries[0].sent.map((m) => m.message.content), ["hello"]);
  assert.deepEqual(signals, ["busy"]);
});

test("a session that dies at boot, before any prompt, shows the failure and the next prompt starts under a fresh id", async () => {
  const { agent, sdk, events, signals, out } = makeAgent({}, [new Error("Claude Code process exited with code 1")]);
  const first = agent.sid;
  agent.ensureQuery();
  await tick();
  const msgs = agent.transcript.messages();
  assert.equal(msgs.length, 1, "the failure has a turn of its own");
  assert.equal(msgs[0].info.role, "assistant");
  assert.equal(msgs[0].info.error.data.message, "Claude Code process exited with code 1");
  assert.deepEqual(events.map((e) => e.type), ["message.updated", "session.error", "session.idle", "session.created"]);
  assert.ok(out.includes("! Claude Code process exited with code 1\n"));
  assert.deepEqual(signals, [], "no turn was running, so no busy or idle was signalled");
  assert.equal(agent.q, null);
  assert.equal(agent.feed, null);
  assert.notEqual(agent.sid, first, "the id that may already be taken on disk is not reused");
  assert.equal(agent.transcript.sessionID, agent.sid);
  assert.equal(events[3].properties.info.id, agent.sid, "the chat is told which conversation to follow");
  agent.prompt("hello");
  assert.equal(sdk.queries.length, 2);
  assert.equal(sdk.queries[1].options.sessionId, agent.sid, "started fresh under the new id, not resumed");
  assert.equal(sdk.queries[1].options.resume, undefined);
});

test("a session that took a prompt but died before replying shows the failure after the prompt", async () => {
  const { agent, sdk } = makeAgent({}, [new Error("auth failed")]);
  const first = agent.sid;
  agent.prompt("hello");
  await tick();
  const msgs = agent.transcript.messages();
  assert.deepEqual(msgs.map((m) => m.info.role), ["user", "assistant"]);
  assert.equal(msgs[1].info.error.data.message, "auth failed");
  assert.notEqual(agent.sid, first, "nothing replied, so the next start gets a fresh id");
  agent.prompt("again");
  assert.equal(sdk.queries[1].options.sessionId, agent.sid);
});

test("a session that replied and then died while idle keeps that reply clean and is continued", async () => {
  const { agent, sdk } = makeAgent({}, [
    { type: "assistant", message: { id: "m1", content: [{ type: "text", text: "done" }] } },
    { type: "result", subtype: "success", is_error: false },
    new Error("Claude Code process exited with code 1"),
  ]);
  const sid = agent.sid;
  agent.prompt("go");
  await tick();
  const msgs = agent.transcript.messages();
  assert.deepEqual(msgs.map((m) => m.info.role), ["user", "assistant", "assistant"]);
  assert.equal(msgs[1].info.error, undefined, "the reply that finished is not the one that failed");
  assert.equal(msgs[2].info.error.data.message, "Claude Code process exited with code 1");
  assert.equal(agent.sid, sid, "a conversation with a reply on disk keeps its id");
  agent.prompt("more");
  assert.equal(sdk.queries[1].options.resume, sid, "and is continued");
});

test("a resumed session that dies at boot leaves the earlier conversation's replies alone", async () => {
  const { agent, sdk } = makeAgent({ session: "ses-old" }, [new Error("auth failed")]);
  sdk.stored["ses-old"] = [
    { type: "user", uuid: "u1", message: { role: "user", content: "earlier" } },
    { type: "assistant", message: { id: "m1", content: [{ type: "text", text: "fine yesterday" }] } },
  ];
  await agent.preload();
  agent.ensureQuery();
  await tick();
  const msgs = agent.transcript.messages();
  assert.equal(msgs.length, 3);
  assert.equal(msgs[1].info.error, undefined, "yesterday's reply is not the one that failed");
  assert.equal(msgs[2].info.error.data.message, "auth failed");
  assert.equal(agent.sid, "ses-old", "a conversation on disk keeps its id");
  agent.prompt("again");
  assert.equal(sdk.queries[1].options.resume, "ses-old", "a conversation that exists on disk is still continued");
});

test("a prompt starts the query, feeds it, and signals busy; the result signals idle", async () => {
  const { agent, sdk, signals, events, out } = makeAgent({ systemPromptFile: "/base/AGENTS.md", model: "opus", allowed: ["Read"] });
  agent.prompt("hello");
  assert.equal(sdk.queries.length, 1);
  const q = sdk.queries[0];
  assert.equal(q.options.sessionId, agent.sid);
  assert.equal(q.options.resume, undefined, "a fresh start pins a new id");
  assert.equal(q.options.systemPrompt.append, "# base\n");
  assert.equal(q.options.model, "opus");
  assert.deepEqual(q.options.allowedTools, ["Read", "mcp__claude-peers"], "the peers shim's tools never raise a card");
  assert.equal(q.options.mcpServers, undefined, "no mcp file, no servers option");
  assert.equal(q.options.disallowedTools, undefined, "an empty deny list is not sent");
  await tick();
  assert.equal(q.sent[0].message.content, "hello");
  assert.deepEqual(signals, ["busy"]);
  assert.equal(events[0].type, "message.updated");
  assert.equal(events[0].properties.info.role, "user");
  assert.ok(out.includes("> hello\n"));
  agent.onResult({ type: "result", subtype: "success", is_error: false });
  await tick();
  assert.deepEqual(signals, ["busy", "idle"]);
  assert.equal(events[events.length - 1].type, "session.idle");
  assert.equal(agent.busy, false);
});

test("interrupt stops a running turn: pending cards are denied, the SDK is told, and the turn's error is not reported", async () => {
  const { agent, sdk, events } = makeAgent();
  agent.prompt("do things");
  const p = agent.canUseTool("Bash", { command: "ls" }, { toolUseID: "c1" });
  const pq = agent.canUseTool("AskUserQuestion", { questions: [] }, { toolUseID: "c2" });
  await agent.interrupt();
  assert.equal(sdk.queries[0].interrupts, 1);
  assert.equal((await p).behavior, "deny");
  assert.equal((await pq).behavior, "deny");
  assert.equal(agent.pendingPermissions().length + agent.pendingQuestions().length, 0);
  agent.onResult({ type: "result", subtype: "error_during_execution", is_error: true, result: "interrupted" });
  assert.equal(events.some((e) => e.type === "session.error"), false, "an interrupted turn is not an error");
  assert.equal(events[events.length - 1].type, "session.idle");
});

test("a Stop that lands after the turn ended is a no-op and the next failing turn still reports", async () => {
  const { agent, sdk, events, out } = makeAgent();
  agent.prompt("one");
  agent.onResult({ type: "result", subtype: "success", is_error: false });
  await agent.interrupt();
  assert.equal(sdk.queries[0].interrupts, 0, "nothing was running");
  agent.prompt("two");
  agent.onResult({ type: "result", subtype: "error", is_error: true, result: "rate limited" });
  const errs = events.filter((e) => e.type === "session.error");
  assert.equal(errs.length, 1);
  assert.equal(errs[0].properties.error, "rate limited");
  assert.ok(out.includes("! rate limited\n"));
});

test("an interrupt the SDK refuses does not leave the flag set", async () => {
  const { agent, sdk, events } = makeAgent();
  agent.prompt("one");
  sdk.queries[0].interruptFails = true;
  await assert.rejects(() => agent.interrupt(), /no turn/);
  agent.onResult({ type: "result", subtype: "error", is_error: true, result: "overloaded" });
  assert.equal(events.filter((e) => e.type === "session.error").length, 1);
});

test("a query that throws ends the turn with an error and idle, and the next prompt starts a new query that resumes", async () => {
  const { agent, sdk, events, signals } = makeAgent({}, [
    { type: "assistant", message: { id: "m1", content: [{ type: "text", text: "partial" }] } },
    new Error("connection lost"),
  ]);
  agent.prompt("go");
  await tick();
  const kinds = events.map((e) => e.type);
  assert.ok(kinds.includes("session.error") && kinds[kinds.length - 1] === "session.idle", kinds.join(","));
  const failed = events.find((e) => e.type === "message.updated" && e.properties.info.role === "assistant");
  assert.equal(failed && agent.transcript.messages()[1].info.error.data.message, "connection lost");
  assert.equal(agent.q, null);
  assert.equal(agent.feed, null, "the dead query's feed is dropped");
  assert.deepEqual(signals, ["busy", "idle"]);
  assert.deepEqual(sdk.queries[0].sent.map((m) => m.message.content), ["go"]);
  agent.prompt("again");
  assert.equal(sdk.queries.length, 2);
  assert.equal(sdk.queries[1].options.resume, agent.sid, "the same conversation continues");
  await tick();
  assert.deepEqual(sdk.queries[1].sent.map((m) => m.message.content), ["again"], "the new query gets the prompt, not the dead generator");
  assert.deepEqual(sdk.queries[0].sent.map((m) => m.message.content), ["go"], "the dead generator took nothing");
});

test("sessions lists the store plus the current one before its first turn, and a broken store throws", async () => {
  const { agent, sdk } = makeAgent({ session: "ses-old" });
  sdk.sessions = [{ sessionId: "ses-old", summary: "Old", cwd: "/inst", lastModified: 5 }];
  sdk.stored["ses-old"] = [{ type: "user", uuid: "u1", message: { role: "user", content: "earlier" } }];
  await agent.preload();
  assert.equal(agent.transcript.messages()[0].parts[0].text, "earlier");
  const list = await agent.sessions();
  assert.deepEqual(list.map((s) => s.id), ["ses-old"], "a resumed session is not listed twice");
  const fresh = makeAgent().agent;
  assert.deepEqual((await fresh.sessions()).map((s) => s.id), [fresh.sid], "a fresh session appears before it is on disk");
  sdk.listFails = true;
  await assert.rejects(() => agent.sessions(), /store locked/);
});

test("a daemon that refuses or cannot be reached is said once in the terminal, and the turn goes on", async () => {
  const bad = makeAgent({ signalStatus: 404 });
  await bad.agent.signal("busy");
  await bad.agent.signal("idle");
  const said = bad.out.filter((l) => l.startsWith("! daemon signal"));
  assert.equal(said.length, 1, "one line per outage, not per signal");
  assert.match(said[0], /busy: HTTP 404/);
  const down = makeAgent({ signalFails: true });
  down.agent.prompt("go");
  await tick();
  assert.equal(down.out.filter((l) => l.includes("ECONNREFUSED")).length, 1);
  assert.equal(down.agent.busy, true, "the turn is not stopped by a deaf daemon");
  const fine = makeAgent();
  await fine.agent.signal("busy");
  assert.equal(fine.out.filter((l) => l.startsWith("! daemon signal")).length, 0);
});

test("messages for another session on disk go through the store; shutdown ends the streams and signals idle", async () => {
  const { agent, sdk, signals } = makeAgent();
  sdk.stored.other = [{ type: "assistant", message: { id: "m9", content: [{ type: "text", text: "old reply" }] } }];
  const msgs = await agent.messages("other");
  assert.equal(msgs[0].parts[0].text, "old reply");
  let ended = 0;
  agent.clients.add({ write() {}, end: () => ended++ });
  await agent.shutdown();
  assert.equal(ended, 1);
  assert.deepEqual(signals, ["idle"]);
  assert.ok(agent.abort.signal.aborted);
});

test("a card carries a five-letter bus id and is relayed to the daemon; a failed relay is said once", async () => {
  for (let i = 0; i < 50; i++) assert.match(askID(), /^[a-km-z]{5}$/);
  const { agent, asks, events, signals, headers, out } = makeAgent();
  const p = agent.canUseTool("Bash", { command: "printf hi" }, { toolUseID: "c1", description: "print a greeting" });
  const q = agent.canUseTool("AskUserQuestion", { questions: [
    { question: "Which tone?", header: "Tone", options: [{ label: "Warm", description: "friendly" }, { label: "Dry" }] },
    { question: "Post now?", header: "", options: [{ label: "Yes" }], multiSelect: true },
  ] }, { toolUseID: "c2" });
  await tick();
  const [perm, ques] = events.filter((e) => e.type.endsWith(".asked")).map((e) => e.properties);
  assert.match(perm.id, /^[a-km-z]{5}$/);
  assert.match(ques.id, /^[a-km-z]{5}$/);
  assert.notEqual(perm.id, ques.id);
  assert.deepEqual(asks, [
    { kind: "permission", id: perm.id, tool: "Bash", description: "print a greeting", preview: "printf hi" },
    { kind: "question", id: ques.id, text: "Tone: Which tone?\n  - Warm: friendly\n  - Dry\nPost now? (one or more)\n  - Yes" },
  ]);
  assert.deepEqual(signals, ["permission", "question"], "the relay is not a signal");
  for (const h of headers) assert.equal(h.authorization, "Bearer tok-p1", "every daemon call carries the pane token");
  assert.equal(out.filter((l) => l.includes(`card ${perm.id} relayed to 1 peer`)).length, 1);
  // The routes answer by the same id, so a bus reply the daemon hands over lands on the card.
  assert.equal(agent.replyPermission(perm.id, "once"), true);
  assert.equal(agent.replyQuestion(ques.id, [["Dry"], ["Yes"]]), true);
  assert.equal((await p).behavior, "allow");
  assert.deepEqual((await q).updatedInput.answers, { "Which tone?": "Dry", "Post now?": ["Yes"] });

  // No daemon in the environment: no relay, no failure said. A daemon that
  // refuses: said once, the card stays up.
  const nobody = makeAgent({ relayedTo: 0 });
  nobody.agent.canUseTool("Bash", { command: "ls" }, { toolUseID: "c9" });
  await tick();
  assert.equal(nobody.out.filter((l) => l.includes("reached no peer")).length, 1, "a card nobody got is said, not silent");
  const alone = makeAgent({ env: { CCMUX_DAEMON_URL: "", CCMUX_PANE_ID: "", CCMUX_PANE_TOKEN: "" } });
  alone.agent.canUseTool("Bash", { command: "ls" }, { toolUseID: "c3" });
  await tick();
  assert.deepEqual(alone.asks, []);
  assert.deepEqual(alone.out.filter((l) => l.includes("card relay")), []);
  const refused = makeAgent({ askStatus: 404 });
  refused.agent.canUseTool("Bash", { command: "ls" }, { toolUseID: "c4" });
  refused.agent.canUseTool("Bash", { command: "pwd" }, { toolUseID: "c5" });
  await tick();
  assert.equal(refused.asks.length, 2);
  const said = refused.out.filter((l) => l.includes("card relay"));
  assert.equal(said.length, 1, "one line per outage");
  assert.match(said[0], /HTTP 404: no peer is registered on this pane/, "the daemon's reason is in the line");
  assert.match(said[0], /until the daemon answers again/);
  const stale = makeAgent({ askStatus: 401 });
  stale.agent.canUseTool("Bash", { command: "ls" }, { toolUseID: "c6" });
  await tick();
  assert.match(stale.out.find((l) => l.includes("card relay")), /HTTP 401: invalid pane token \(restart this agent/, "a refused token is not an outage");
  assert.equal(refused.agent.pendingPermissions().length, 2, "the cards stay up in the chat");
});

test("the base's mcp.json reaches the SDK as servers, expanded from the environment, with their tools allowed", () => {
  const files = {
    "/base/AGENTS.md": "# base\n",
    "/base/mcp.json": JSON.stringify({ door: { type: "http", url: "${X_MCP_URL}" }, kb: { command: "kb-mcp" } }),
  };
  const readFile = (p) => { if (files[p] === undefined) { const e = new Error("ENOENT"); e.code = "ENOENT"; throw e; } return files[p]; };
  const { agent, sdk } = makeAgent({ systemPromptFile: "/base/AGENTS.md", mcpFiles: ["/base/mcp.json"], allowed: ["Read"], readFile, env: { X_MCP_URL: "http://door/mcp" } });
  agent.prompt("hi");
  const o = sdk.queries[0].options;
  assert.deepEqual(o.mcpServers, { door: { type: "http", url: "http://door/mcp" }, kb: { type: "stdio", command: "kb-mcp", args: [] } });
  assert.deepEqual(o.allowedTools, ["Read", "mcp__claude-peers", "mcp__door", "mcp__kb"]);

  // A base without the file: no servers, the peers rule still there.
  const bare = makeAgent({ mcpFiles: ["/base/none.json"], readFile }).agent;
  assert.deepEqual(bare.mcp, { servers: {}, allow: ["mcp__claude-peers"], unresolved: {} });
  // A reference the .env does not set is said in the terminal at start.
  const { out: out2 } = makeAgent({ mcpFiles: ["/base/mcp.json"], readFile, env: {} });
  assert.ok(out2.some((l) => l.includes("! mcp door: ${X_MCP_URL} not set")), out2.join(""));
  // A file that does not parse ends the start, like an unreadable AGENTS.md;
  // so does one that cannot be read for any reason but absence.
  assert.throws(() => makeAgent({ mcpFiles: ["/base/bad.json"], readFile: () => "{nope" }), SyntaxError);
  const denied = () => { const e = new Error("EACCES: permission denied"); e.code = "EACCES"; throw e; };
  assert.throws(() => makeAgent({ mcpFiles: ["/base/mcp.json"], readFile: denied }), /EACCES/);
});

test("a runner with a pane starts the shim as its pane's bus owner; one without a pane, and an agent pane, do not", () => {
  const shim = (opts) => {
    const { agent, sdk } = makeAgent({ peersCommand: "/bin/ccmux-peers", ...opts });
    agent.ensureQuery();
    return sdk.queries[0].options.mcpServers["claude-peers"];
  };
  assert.deepEqual(shim({ socket: "/run/s.sock" }).env, { CCMUX_BUS_OWNER: "1" });
  assert.equal(shim({ socket: "/run/s.sock", env: { CCMUX_PANE_ID: "" } }).env, undefined, "no pane: nothing on the bus to own");
  assert.equal(shim({}).env, undefined, "an agent pane's shim owns by its terminal");
});

test("an isolated start names its setting sources, every plugin folder and its skills; any other start sends none of them", () => {
  const { agent, sdk } = makeAgent({ pluginDirs: ["/base", "/plugins/kb"], settingSources: ["project"], skills: ["radar:scan"], peersCommand: "/bin/ccmux-peers" });
  agent.prompt("hi");
  const o = sdk.queries[0].options;
  assert.deepEqual(o.plugins, [{ type: "local", path: "/base" }, { type: "local", path: "/plugins/kb" }]);
  assert.deepEqual(o.settingSources, ["project"]);
  assert.equal(o.env.ENABLE_CLAUDEAI_MCP_SERVERS, "false", "the login's claude.ai connectors are off too");
  assert.equal(o.env.CCMUX_PANE_ID, "p1", "the rest of the environment rides along");
  assert.deepEqual(o.skills, ["radar:scan"]);
  assert.deepEqual(o.mcpServers, { "claude-peers": { type: "stdio", command: "/bin/ccmux-peers", args: [] } });
  const plain = makeAgent({ pluginDirs: ["/base"] });
  plain.agent.prompt("hi");
  const p = plain.sdk.queries[0].options;
  assert.equal(p.settingSources, undefined, "not isolated: the SDK's default, every source");
  assert.equal(p.env, undefined);
  assert.equal(p.skills, undefined);
  assert.equal(p.mcpServers, undefined);
});

test("an isolated agent's always is saved where its next start reads it: project settings, not local ones", async () => {
  const { agent } = makeAgent({ settingSources: ["project"] });
  const local = { type: "addRules", rules: [{ toolName: "Bash", ruleContent: "npm test" }], behavior: "allow", destination: "localSettings" };
  const session = { type: "addRules", rules: [{ toolName: "Read" }], behavior: "allow", destination: "session" };
  const p = agent.canUseTool("Bash", { command: "npm test" }, { toolUseID: "c1", suggestions: [local, session] });
  const [id] = [...agent.permissions.keys()];
  agent.replyPermission(id, "always");
  const res = await p;
  assert.deepEqual(res.updatedPermissions, [{ ...local, destination: "projectSettings" }, session]);
  const plain = makeAgent({});
  const p2 = plain.agent.canUseTool("Bash", { command: "npm test" }, { toolUseID: "c2", suggestions: [local] });
  plain.agent.replyPermission([...plain.agent.permissions.keys()][0], "always");
  assert.deepEqual((await p2).updatedPermissions, [local], "not isolated: as the SDK suggested");
});

test("walls: a write inside the roots skips the card, one outside is refused with none, the rest still ask", async () => {
  const { agent, asks, events } = makeAgent({ writeRoots: ["/inst", "/win/shared/probe"], writeInside: "allow" });
  assert.deepEqual(await agent.canUseTool("Write", { file_path: "/inst/log.md" }, { toolUseID: "w1" }),
    { behavior: "allow", updatedInput: { file_path: "/inst/log.md" } });
  const out = await agent.canUseTool("Edit", { file_path: "/repo/app/x.ts" }, { toolUseID: "w2" });
  assert.equal(out.behavior, "deny");
  assert.match(out.message, /Walls: .*\/repo\/app\/x\.ts is outside/);
  assert.equal(asks.length, 0, "a walled write raises no card on the bus");
  assert.equal(events.filter((e) => e.type === "permission.asked").length, 0, "nor in the chat");
  agent.canUseTool("Bash", { command: "ls" }, { toolUseID: "w3" }); // not a file write: the card as before
  await tick();
  assert.equal(events.filter((e) => e.type === "permission.asked").length, 1);
});

test("walls: a card for a write inside the roots never offers always (its rule would lift the walls)", async () => {
  const { agent, events } = makeAgent({ writeRoots: ["/inst"] }); // edit gate ask: the card stays
  const S = { type: "setMode", mode: "acceptEdits", destination: "session" };
  agent.canUseTool("Write", { file_path: "/inst/log.md" }, { toolUseID: "a1", suggestions: [S] });
  const shellRule = { type: "addRules", rules: [{ toolName: "Bash", ruleContent: "npm test" }], behavior: "allow", destination: "session" };
  const editRule = { type: "addRules", rules: [{ toolName: "Edit" }], behavior: "allow", destination: "session" };
  agent.canUseTool("Bash", { command: "ls" }, { toolUseID: "a2", suggestions: [S, editRule] }); // both would lift the walls
  agent.canUseTool("Bash", { command: "npm test" }, { toolUseID: "a3", suggestions: [S, shellRule] });
  await tick();
  const asked = events.filter((e) => e.type === "permission.asked").map((e) => e.properties);
  assert.deepEqual(asked.map((r) => r.always), [[], [], ["*"]]);
  const pending = [...agent.permissions.values()];
  assert.deepEqual(pending[2].suggestions, [shellRule], "a shell rule stays; the mode switch goes");
});

test("walls: a PreToolUse hook refuses writes outside the roots before any saved allow rule; no walls, no hook", async () => {
  const { agent } = makeAgent({ writeRoots: ["/inst", "/win/shared/probe"], writeInside: "allow" });
  const [m] = agent.options().hooks.PreToolUse;
  assert.equal(m.matcher, "Edit|Write|MultiEdit|NotebookEdit");
  const hook = m.hooks[0];
  const out = await hook({ tool_name: "Write", tool_input: { file_path: "/ext/projects/chartlabs/hq/ideas/x.md" } });
  assert.equal(out.hookSpecificOutput.permissionDecision, "deny");
  assert.match(out.hookSpecificOutput.permissionDecisionReason, /^Walls: /);
  assert.deepEqual(await hook({ tool_name: "Write", tool_input: { file_path: "/inst/log.md" } }), {}, "inside a root the hook decides nothing");
  assert.deepEqual(await hook({ tool_name: "NotebookEdit", tool_input: { notebook_path: "/win/shared/probe/n.ipynb" } }), {});
  const { agent: plain } = makeAgent({ writeRoots: [] });
  assert.equal(plain.options().hooks, undefined);
});

// --- the session runner (--socket, --state, --session-auto, --claude) ---

// runnerAgent is an agent started the way internal/runner starts one, with
// its state file kept in memory.
function runnerAgent(opts = {}, script = []) {
  const files = {};
  const made = makeAgent({ socket: "/run/s.sock", state: "/st/s.json", sessionAuto: "sess-1", claude: "/bin/claude",
    settingSources: ["user", "project", "local"], readFile: (p) => { if (p in files) return files[p]; const e = new Error("no"); e.code = "ENOENT"; throw e; }, ...opts }, script);
  made.agent.sdk.writeFile = (p, t) => { files[p] = t; };
  made.agent.sdk.rename = (a, b) => { files[b] = files[a]; delete files[a]; };
  made.sdk.exits = [];
  made.agent.sdk.exit = (c) => made.sdk.exits.push(c);
  // A runner's stream puts an id line before each data line.
  made.agent.clients.clear();
  made.agent.clients.add({ write: (line) => made.events.push(JSON.parse(line.split("data: ")[1])), end() {} });
  return { ...made, files };
}

test("a runner loads the user's own settings, keeps their connectors and runs the named claude", () => {
  const { agent, sdk } = runnerAgent();
  agent.prompt("hi", "p1");
  const o = sdk.queries[0].options;
  assert.deepEqual(o.settingSources, ["user", "project", "local"]);
  assert.equal(o.env, undefined, "connectors stay on for the user's own session");
  assert.equal(o.pathToClaudeCodeExecutable, "/bin/claude");
  assert.equal(o.permissionMode, undefined, "the user's settings choose the mode");
  const pane = makeAgent();
  pane.agent.prompt("hi");
  assert.equal(pane.sdk.queries[0].options.permissionMode, "default", "an agent pane keeps its fixed mode");
  assert.equal(o.effort, undefined, "and their settings choose the effort");
});

test("a runner keeps file checkpoints, and once it has gone back it takes no prompt until it starts again", async () => {
  const { agent, sdk } = runnerAgent();
  assert.equal(agent.prompt("hi", "p1"), true);
  assert.equal(sdk.queries[0].options.enableFileCheckpointing, true);
  agent.rewinding = true;
  assert.equal(agent.hello().busy, true, "busy to the daemon while it goes back, so no restart cuts it");
  agent.rewinding = false;
  agent.rewound = true;
  assert.equal(agent.prompt("more", "p2"), false, "the daemon keeps it for the fork's start");
  await tick();
  assert.deepEqual(sdk.queries[0].sent.map((m) => m.message.content), ["hi"]);
  const pane = makeAgent();
  pane.agent.prompt("hi");
  assert.equal(pane.sdk.queries[0].options.enableFileCheckpointing, undefined, "an agent pane keeps none");
  await assert.rejects(pane.agent.rewind("x", true), /only a session runner/);
});

test("a runner asks for a predicted next prompt, passes it on and in its snapshot, and a prompt spends it", async () => {
  const { agent, sdk, events } = runnerAgent();
  agent.prompt("hi", "p1");
  assert.equal(sdk.queries[0].options.promptSuggestions, true);
  agent.onMessage({ type: "prompt_suggestion", suggestion: "run the tests", uuid: "u1", session_id: "s" });
  const said = () => events.filter((e) => e.type === "session.suggestion").map((e) => e.properties.suggestion);
  assert.deepEqual(said(), ["run the tests"]);
  assert.equal(agent.snapshot().suggestion, "run the tests", "a page that opens now gets it too");
  agent.prompt("something else", "p2");
  assert.deepEqual(said(), ["run the tests", ""], "a prompt clears it");
  assert.equal(agent.snapshot().suggestion, "");
  const pane = makeAgent();
  pane.agent.prompt("hi");
  assert.equal(pane.sdk.queries[0].options.promptSuggestions, undefined, "an agent pane asks for none");
  pane.agent.onMessage({ type: "prompt_suggestion", suggestion: "x" });
  assert.equal(pane.events.filter((e) => e.type === "session.suggestion").length, 0);
});

test("a task's builder runs in the mode and at the effort its row names", () => {
  const { agent, sdk } = runnerAgent({ permissionMode: "auto", effort: "medium", model: "opus" });
  agent.prompt("build it", "p1");
  const o = sdk.queries[0].options;
  assert.equal(o.permissionMode, "auto", "nobody is there to answer a permission prompt");
  assert.equal(o.effort, "medium");
  assert.equal(o.model, "opus");
  // An agent pane's mode is its base's, whatever the line says.
  const pane = makeAgent({ permissionMode: "auto" });
  pane.agent.prompt("hi");
  assert.equal(pane.sdk.queries[0].options.permissionMode, "default");
});

// A role agent that runs as a session (the daemon's runner/agent.go): the
// runner's own flags with its base's after them, parsed from one line as the
// daemon writes it.
test("a role agent's session runs as its pane did: its base's plugin, prompt, gates and walls, asking by its base's rules", async () => {
  const line = [
    "--socket", "/run/s.sock", "--state", "/st/c1.state.json", "--session-auto", "c1", "--detached", "1",
    "--setting-sources", "user,project,local", "--agent", "radar", "--media", "/st/s.media",
    "--model", "sonnet", "--peers-command", "/bin/ccmux-peers", "--permission-mode", "default",
    "--plugin-dir", "/agents/radar", "--system-prompt-file", "/agents/radar/AGENTS.md", "--mcp-file", "/agents/radar/mcp.json",
    "--add-dir", "/repo", "--allowed-tools", "Glob,Grep,Read", "--write-root", "/w/agents/radar", "--write-inside", "allow",
  ];
  const { agent, sdk } = runnerAgent({ ...parseArgs(line), claude: undefined, readFile: (p) => (p.endsWith("AGENTS.md") ? "# radar\n" : "{}") });
  agent.prompt("the morning run", "p1");
  const o = sdk.queries[0].options;
  assert.equal(o.permissionMode, "default", "it asks by its base's gates, whatever the person's settings say");
  assert.equal(o.pathToClaudeCodeExecutable, undefined, "the SDK's own Claude Code, as in its pane");
  assert.deepEqual(o.plugins, [{ type: "local", path: "/agents/radar" }]);
  assert.equal(o.systemPrompt.append, "# radar\n");
  assert.equal(o.model, "sonnet");
  assert.deepEqual(o.additionalDirectories, ["/repo"]);
  assert.deepEqual(o.allowedTools, ["Glob", "Grep", "Read", "mcp__claude-peers"]);
  assert.deepEqual(o.mcpServers["claude-peers"], { type: "stdio", command: "/bin/ccmux-peers", args: [], env: { CCMUX_BUS_OWNER: "1" } }, "the daemon's shim, as its pane's bus owner");
  assert.equal(o.hooks.PreToolUse.length, 1, "its walls hold in a session too");
  assert.equal(o.sessionId, "c1", "the conversation its row names");
  // An isolated base's setting sources come after the runner's own on the
  // line, and win: the home's project settings only, connectors off.
  const isolated = runnerAgent({ ...parseArgs([...line, "--setting-sources", "project"]), readFile: () => "{}" });
  isolated.agent.prompt("go", "p1");
  const io = isolated.sdk.queries[0].options;
  assert.deepEqual(io.settingSources, ["project"]);
  assert.equal(io.env.ENABLE_CLAUDEAI_MCP_SERVERS, "false");
  assert.equal(io.mcpServers["claude-peers"].command, "/bin/ccmux-peers", "the bus is still the daemon's shim");
});

// --- a runner's loadout (--loadout, loadout.mjs) ---

const LOADOUT = "/st/s.loadout.json";

// loadoutAgent is a runner handed a loadout file.
function loadoutAgent(loadout, opts = {}) {
  const body = typeof loadout === "string" ? loadout : JSON.stringify({ pluginDirs: [], enabledPlugins: {}, skills: [], deniedServers: [], connectors: false, mcp: {}, ...loadout });
  return runnerAgent({ loadout: LOADOUT, readFile: (p) => { if (p === LOADOUT) return body; const e = new Error("no"); e.code = "ENOENT"; throw e; }, ...opts });
}

test("a runner with a loadout loads what its group and repo chose, on top of the user's own settings", () => {
  const { agent, sdk, out } = loadoutAgent({
    pluginDirs: ["/caps/personal/plugins/kb"], enabledPlugins: { "qa@personal": true, "marketing@synced": false, "stripe@official": false },
    skills: ["qa:check", "kb:write", "glitchtip"], deniedServers: ["claude.ai Gmail", "claude_design"], connectors: true,
  }, { peersCommand: "/bin/ccmux-peers" });
  agent.prompt("hi", "p1");
  const o = sdk.queries[0].options;
  assert.deepEqual(o.settingSources, ["user", "project", "local"], "CLAUDE.md and memory still come from the user's settings");
  assert.deepEqual(o.plugins, [{ type: "local", path: "/caps/personal/plugins/kb" }]);
  assert.deepEqual(o.settings, {
    enabledPlugins: { "qa@personal": true, "marketing@synced": false, "stripe@official": false },
    deniedMcpServers: [{ serverName: "claude.ai Gmail" }, { serverName: "claude_design" }],
  });
  assert.deepEqual(o.skills, ["qa:check", "kb:write", "glitchtip"]);
  assert.equal(o.env, undefined, "a connector is chosen, so connectors stay on");
  assert.equal(o.strictMcpConfig, undefined, "never: it would drop the bus");
  assert.equal(o.mcpServers["claude-peers"].command, "/bin/ccmux-peers", "the bus is the fixed base");
  assert.ok(out.join("").includes("loadout: 2 plugin(s) on, 2 off"), "the start says what it was given");
});

test("a runner whose group and repo chose nothing offers no skill and switches connectors off, and keeps the bus", () => {
  const { agent, sdk } = loadoutAgent({}, { peersCommand: "/bin/ccmux-peers", env: { HOME: "/h" } });
  agent.prompt("hi", "p1");
  const o = sdk.queries[0].options;
  assert.deepEqual(o.skills, [], "an empty list, not a missing one: missing would offer every skill");
  assert.equal(o.env.ENABLE_CLAUDEAI_MCP_SERVERS, "false");
  assert.equal(o.env.HOME, "/h");
  assert.equal(o.plugins, undefined);
  assert.equal(o.settings, undefined);
  assert.deepEqual(Object.keys(o.mcpServers), ["claude-peers"]);
});

test("a loadout's catalog servers are passed, and never in the bus's place", () => {
  const { agent, sdk } = loadoutAgent({
    deniedServers: ["claude-peers"], mcp: { search: { type: "http", url: "https://s/mcp" }, "claude-peers": { command: "/evil" } },
  }, { peersCommand: "/bin/ccmux-peers" });
  agent.prompt("hi", "p1");
  const o = sdk.queries[0].options;
  assert.deepEqual(o.mcpServers.search, { type: "http", url: "https://s/mcp" });
  assert.equal(o.mcpServers["claude-peers"].command, "/bin/ccmux-peers");
  assert.equal(o.settings, undefined, "the bus was the only name denied, and it is never denied");
  assert.ok(o.allowedTools.includes("mcp__search"), "a chosen server's tools ask nothing, as an agent's do");
});

test("a loadout file that is missing or malformed ends the start instead of loading everything", () => {
  assert.throws(() => runnerAgent({ loadout: LOADOUT }), /no/, "the file is not there");
  assert.throws(() => loadoutAgent(JSON.stringify({ pluginDirs: [] })), /enabledPlugins/);
  assert.throws(() => loadoutAgent("{ not json"));
});

test("a prompt goes in under its inbox id, and the same id is never sent twice", async () => {
  const { agent, sdk } = runnerAgent();
  assert.equal(agent.prompt("hi", "p1"), true);
  assert.equal(agent.prompt("hi", "p1"), false, "a resend of a prompt already sent");
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(sdk.queries[0].sent.map((m) => m.uuid), ["p1"]);
});

test("a runner whose Claude dies ends itself, keeping its session id; an agent pane goes on", async () => {
  const { agent, sdk } = runnerAgent();
  await agent.preload();
  agent.prompt("hi", "p1");
  sdk.queries[0].ended(); // the Claude child exits before replying
  await tick();
  assert.deepEqual(sdk.exits, [1], "systemd restarts it, and the start resumes");
  assert.equal(agent.sid, "sess-1", "never a new id: the row and the transcript stay matched");
  const pane = makeAgent();
  pane.sdk.exits = [];
  pane.agent.sdk.exit = (c) => pane.sdk.exits.push(c);
  pane.agent.prompt("hi");
  pane.sdk.queries[0].ended();
  await tick();
  assert.deepEqual(pane.sdk.exits, [], "an agent pane is not ended by a failed query");
});

test("a second prompt queued behind a turn keeps the runner busy past the first result", () => {
  const { agent } = runnerAgent();
  agent.prompt("fix it", "p1");
  agent.prompt("and commit", "p2");
  agent.onMessage({ type: "assistant", user_message_uuid: "p1", message: { content: [] } });
  agent.onResult({ type: "result" });
  assert.equal(agent.busy, true);
  assert.equal(agent.hello().turnOpen, true);
  agent.onMessage({ type: "assistant", user_message_uuid: "p2", message: { content: [] } });
  agent.onResult({ type: "result" });
  assert.equal(agent.busy, false);
});

test("a reply stamped with a prompt's id marks it delivered, and the state file says so", () => {
  const { agent, files } = runnerAgent();
  agent.prompt("hi", "p1");
  agent.onMessage({ type: "assistant", user_message_uuid: "p1", message: { content: [] } });
  assert.deepEqual(agent.delivered(["p1", "p2"]), ["p1"]);
  assert.deepEqual(JSON.parse(files["/st/s.json"]).delivered, ["p1"]);
  assert.deepEqual(JSON.parse(files["/st/s.json"]).open.map((p) => p.id), ["p1"], "the prompt is open until its result");
  agent.onResult({ type: "result" });
  assert.deepEqual(JSON.parse(files["/st/s.json"]).open, []);
  assert.equal(agent.busy, false);
});

test("a runner restarted mid-turn resumes its transcript and is told to check before redoing", async () => {
  const state = JSON.stringify({ open: [{ id: "p0", text: "commit the fix" }], delivered: [] });
  const { agent, sdk } = runnerAgent({ readFile: (p) => (p === "/st/s.json" ? state : "# base\n") });
  sdk.stored["sess-1"] = [{ type: "user", uuid: "p0", message: { role: "user", content: "commit the fix" } }];
  await agent.preload();
  assert.equal(agent.resume, true, "a transcript on disk is resumed");
  assert.deepEqual(agent.delivered(["p0"]), ["p0"], "prompts in the transcript count as delivered");
  agent.recover();
  await new Promise((r) => setImmediate(r));
  const sent = sdk.queries[0].sent.map((m) => m.message.content);
  assert.equal(sent.length, 1);
  assert.match(sent[0], /cut off/);
  assert.match(sent[0], /commit the fix/);
  assert.equal(sdk.queries[0].options.resume, "sess-1");
});

test("a runner with no transcript yet opens its session fresh, and a clean stop needs no notice", async () => {
  const { agent, sdk } = runnerAgent();
  await agent.preload();
  assert.equal(agent.resume, false);
  agent.recover();
  assert.equal(sdk.queries.length, 0, "nothing to recover, nothing sent");
  agent.prompt("hi", "p1");
  assert.equal(sdk.queries[0].options.sessionId, "sess-1");
});

test("a runner numbers its events for a reader that reconnects", () => {
  const { agent } = runnerAgent();
  agent.prompt("hi", "p1");
  const { events, gap } = agent.events.after(0);
  assert.equal(gap, false);
  assert.ok(events.length >= 2);
  assert.deepEqual(events.map((x) => x.seq), events.map((_, i) => i + 1));
  assert.equal(agent.hello().seq, events.length);
  assert.equal(agent.hello().protocol, 2);
});

test("a runner being stopped never says idle: nothing between turns, stopped when a turn was cut off", async () => {
  const quiet = runnerAgent();
  await quiet.agent.shutdown();
  assert.deepEqual(quiet.signals, [], "between turns: what it last said stands, and a sleep is not a finished turn");
  const mid = runnerAgent();
  mid.agent.prompt("count to a thousand");
  await mid.agent.shutdown();
  assert.deepEqual(mid.signals, ["busy", "stopped"], "mid-turn: the turn is gone, and so is any card it had up");
  assert.ok(mid.agent.abort.signal.aborted);
});

test("a snapshot is the conversation and the number of the last event in it", () => {
  const { agent } = runnerAgent();
  agent.prompt("hi", "p1");
  const snap = agent.snapshot();
  assert.equal(snap.seq, agent.events.seq, "the events after seq are exactly the ones not in the snapshot");
  assert.equal(snap.boot, agent.boot);
  assert.equal(snap.session, agent.sid);
  assert.equal(snap.busy, true);
  assert.equal(snap.messages.length, 1);
  assert.equal(snap.messages[0].parts[0].text, "hi");
  assert.deepEqual([snap.permissions, snap.questions], [[], []]);
  // Read in one go: an event sent right after is numbered above the
  // snapshot, and is not in it.
  agent.prompt("again", "p2");
  assert.equal(agent.events.after(snap.seq).events.length > 0, true);
  assert.equal(snap.messages.length, 1);
});

test("a snapshot carries the cards that wait", async () => {
  const { agent } = runnerAgent();
  agent.prompt("go", "p1");
  const asked = agent.canUseTool("Bash", { command: "rm -rf x" }, { toolUseID: "c1" });
  const snap = agent.snapshot();
  assert.equal(snap.permissions.length, 1);
  assert.equal(snap.permissions[0].permission, "Bash");
  agent.replyPermission(snap.permissions[0].id, "reject");
  await asked;
  assert.equal(agent.snapshot().permissions.length, 0);
});

const initMsg = (over = {}) => ({ type: "system", subtype: "init", model: "claude-haiku-5-5", claude_code_version: "2.1.325", permissionMode: "auto", mcp_servers: [], slash_commands: ["hello"], terminal_slash_commands: ["doctor"], ...over });
const resultMsg = { type: "result", subtype: "success", total_cost_usd: 0.02, duration_ms: 900, num_turns: 1, usage: { input_tokens: 3, output_tokens: 7 } };

test("a runner tells its page what it runs on, once, and again only when that changes", () => {
  const { agent, events } = runnerAgent();
  agent.onMessage(initMsg());
  agent.onMessage(initMsg()); // the init line comes with every turn
  const said = events.filter((e) => e.type === "session.info");
  assert.equal(said.length, 1);
  assert.equal(said[0].properties.info.model, "claude-haiku-5-5");
  assert.deepEqual(said[0].properties.info.commands, [{ name: "hello", description: "", hint: "" }]);
  agent.onMessage({ type: "system", subtype: "commands_changed", commands: [{ name: "hello", description: "Say hello", argumentHint: "" }] });
  agent.onMessage(initMsg({ model: "claude-opus-5-5" }));
  const later = events.filter((e) => e.type === "session.info");
  assert.equal(later.length, 3);
  assert.equal(later[2].properties.info.model, "claude-opus-5-5");
  assert.equal(later[2].properties.info.commands[0].description, "Say hello", "bare names from init do not wipe the descriptions");
  assert.deepEqual(agent.snapshot().info, later[2].properties.info);
});

test("a runner asks a new query what it offers before any turn", async () => {
  const { agent, sdk, events } = runnerAgent();
  const real = sdk.query;
  agent.sdk.query = (args) => {
    const q = real(args);
    q.initializationResult = async () => ({ commands: [{ name: "cost", description: "What it cost", argumentHint: "" }], models: [{ value: "haiku", displayName: "Haiku 5.5", resolvedModel: "claude-haiku-5-5" }] });
    return q;
  };
  agent.ensureQuery();
  await tick();
  const info = events.find((e) => e.type === "session.info").properties.info;
  assert.deepEqual(info.models, [{ value: "haiku", name: "Haiku 5.5", model: "claude-haiku-5-5" }]);
  assert.equal(info.commands[0].name, "cost");
});

test("a turn no prompt opened makes a runner busy until its result, which carries the bill", async () => {
  const { agent, events, signals } = runnerAgent();
  agent.ensureQuery();
  // A background hook woke the session: a turn starts with no prompt of ours.
  agent.onMessage(initMsg());
  assert.equal(agent.busy, false, "the init line alone is not work");
  agent.onMessage({ type: "assistant", message: { id: "m1", content: [{ type: "text", text: "The review found a bug." }] } });
  assert.equal(agent.busy, true);
  agent.onMessage(resultMsg);
  await tick();
  assert.equal(agent.busy, false);
  assert.deepEqual(signals, ["busy", "idle"]);
  const idle = events.filter((e) => e.type === "session.idle").pop();
  const { endedAt, ...bill } = idle.properties.result;
  assert.deepEqual(bill, { costUSD: 0.02, durationMs: 900, turns: 1, usage: { input: 3, output: 7, cacheRead: 0, cacheWrite: 0 }, cacheTtlMs: 0 });
  assert.equal(typeof endedAt, "number");
  assert.deepEqual(agent.snapshot().result, idle.properties.result);
});

test("after a turn a runner says how full the context window is, in the event and the snapshot", async () => {
  const { agent, events, sdk } = runnerAgent();
  agent.ensureQuery();
  const asked = [];
  sdk.queries[0].getContextUsage = async (o) => { asked.push(o); return { totalTokens: 84000, maxTokens: 200000, percentage: 42 }; };
  agent.onMessage(initMsg());
  agent.onMessage({ type: "assistant", message: { id: "m1", content: [{ type: "text", text: "Done." }] } });
  agent.onMessage(resultMsg);
  await tick();
  assert.deepEqual(asked, [{ detail: "summary" }], "summary: no token-count calls");
  const ev = events.filter((e) => e.type === "session.context").pop();
  assert.deepEqual(ev.properties.context, { percent: 42, used: 84000, max: 200000 });
  assert.deepEqual(agent.snapshot().result.context, ev.properties.context);
});

test("a runner whose SDK cannot say how full the context is goes on without a number", async () => {
  const { agent, events, sdk } = runnerAgent();
  agent.ensureQuery();
  sdk.queries[0].getContextUsage = async () => { throw new Error("not supported"); };
  agent.onMessage(initMsg());
  agent.onMessage({ type: "assistant", message: { id: "m1", content: [{ type: "text", text: "Done." }] } });
  agent.onMessage(resultMsg);
  await tick();
  assert.equal(events.filter((e) => e.type === "session.context").length, 0);
  assert.equal(agent.snapshot().result.context, undefined);
});

test("a runner puts what Claude Code says beside the conversation into it, and its to-do list in the snapshot", () => {
  const { agent, events } = runnerAgent();
  agent.prompt("go", "p1");
  agent.onMessage({ type: "system", subtype: "informational", content: "Stop says: Gate: 1 source file(s) unverified", level: "notice" });
  agent.onMessage({ type: "system", subtype: "thinking_tokens", estimated_tokens: 50 });
  const notes = agent.snapshot().messages.filter((m) => m.info.role === "note");
  assert.equal(notes.length, 1);
  assert.equal(notes[0].parts[0].text, "Stop says: Gate: 1 source file(s) unverified");
  agent.onMessage({ type: "assistant", message: { id: "m1", content: [{ type: "tool_use", id: "c1", name: "TodoWrite", input: { todos: [{ content: "Ship", status: "pending" }] } }] } });
  agent.onMessage({ type: "user", message: { role: "user", content: [{ type: "tool_result", tool_use_id: "c1", content: "ok" }] } });
  assert.deepEqual(agent.snapshot().todos, [{ id: "1", text: "Ship", status: "pending" }]);
  assert.equal(events.filter((e) => e.type === "session.todos").length, 1);
});

test("an agent pane says none of it: its stream is what the chat always read", async () => {
  const { agent, events, signals } = makeAgent();
  agent.onMessage(initMsg());
  agent.onMessage({ type: "system", subtype: "informational", content: "Stop says: no", level: "notice" });
  agent.onMessage({ type: "assistant", message: { id: "m1", content: [{ type: "tool_use", id: "c1", name: "Edit", input: { file_path: "/a", old_string: "x", new_string: "y" } }] } });
  agent.onMessage(resultMsg);
  await tick();
  assert.deepEqual(events.map((e) => e.type), ["message.part.updated", "session.idle"]);
  assert.deepEqual(events[1].properties, { sessionID: agent.sid });
  assert.equal(events[0].properties.part.state.diff, undefined);
  assert.deepEqual(signals, [], "a pane is busy by its prompts only");
});

// mediaAgent is a runner whose media folder holds one picture.
function mediaAgent() {
  const made = runnerAgent({ media: "/m" });
  const name = "0123456789abcdef0123456789abcdef.png";
  const bytes = Buffer.from("\x89PNG the picture", "binary");
  made.agent.media.fs = { mkdir() {}, exists: (p) => p === "/m/" + name, write() {}, read: (p) => { if (p !== "/m/" + name) throw new Error("ENOENT " + p); return bytes; } };
  return { ...made, name, data: bytes.toString("base64") };
}

test("a prompt's pictures reach Claude as image blocks before its text, and the page as file parts", async () => {
  const { agent, sdk, name, data } = mediaAgent();
  assert.equal(agent.prompt("what is wrong here?", "p1", [name]), true);
  await tick();
  assert.deepEqual(sdk.queries[0].sent[0].message.content, [
    { type: "image", source: { type: "base64", media_type: "image/png", data } },
    { type: "text", text: "what is wrong here?" },
  ]);
  const parts = agent.snapshot().messages[0].parts;
  assert.deepEqual(parts.map((p) => p.type), ["text", "file"]);
  assert.deepEqual([parts[1].mime, parts[1].name], ["image/png", name]);
  assert.ok(!JSON.stringify(agent.snapshot()).includes(data), "the bytes are not in the transcript");
});

test("a picture with no words is a prompt, and a plain prompt is still a string", async () => {
  const { agent, sdk, name } = mediaAgent();
  agent.prompt("", "p1", [name]);
  agent.prompt("just words", "p2");
  await tick();
  assert.deepEqual(sdk.queries[0].sent[0].message.content.map((b) => b.type), ["image"]);
  assert.equal(sdk.queries[0].sent[1].message.content, "just words");
});

test("a picture that cannot be read does not hold the prompt back: it goes without it and says so", async () => {
  const { agent, sdk, out, name } = mediaAgent();
  agent.prompt("see these", "p1", [name, "ffffffffffffffffffffffffffffffff.png", "../../etc/passwd"]);
  await tick();
  const content = sdk.queries[0].sent[0].message.content;
  assert.deepEqual(content.map((b) => b.type), ["image", "text"]);
  assert.match(content[1].text, /^see these\n\[ccmux: 2 images attached to this message could not be read/);
  assert.equal(agent.snapshot().messages[0].parts.filter((p) => p.type === "file").length, 1);
  assert.equal(out.filter((l) => l.startsWith("! image ")).length, 2);
  // All of them lost: the words still go, as a string.
  agent.prompt("and this", "p2", ["ffffffffffffffffffffffffffffffff.png"]);
  await tick();
  assert.match(sdk.queries[0].sent[1].message.content, /^and this\n\[ccmux: 1 image attached/);
});

test("an agent pane, which keeps no images, sends the words and says the picture is missing", async () => {
  const { agent, sdk } = makeAgent();
  agent.prompt("look", "", ["0123456789abcdef0123456789abcdef.png"]);
  await tick();
  assert.match(sdk.queries[0].sent[0].message.content, /^look\n\[ccmux: 1 image/);
});

const lifecycle = (id, state) => ({ type: "command_lifecycle", command_uuid: id, state });

test("a prompt folded into a running turn is delivered and done by Claude Code's word alone", async () => {
  // The order SDK 0.3.311 sends (recorded 2026-11-16): B arrives while A's
  // turn runs, the replies to B name no prompt, B completes, then the
  // result, then A completes.
  const { agent, signals, files } = runnerAgent();
  agent.prompt("run the build", "A");
  agent.onMessage(lifecycle("A", "queued"));
  agent.onMessage(lifecycle("A", "started"));
  agent.onMessage({ type: "assistant", user_message_uuid: "A", message: { id: "m1", content: [{ type: "text", text: "building" }] } });
  agent.prompt("also run the tests", "B");
  agent.onMessage(lifecycle("B", "queued"));
  assert.deepEqual(agent.delivered(["A", "B"]), ["A"], "queued is not yet with Claude");
  agent.onMessage(lifecycle("B", "started"));
  assert.deepEqual(agent.delivered(["A", "B"]), ["A", "B"]);
  agent.onMessage({ type: "assistant", message: { id: "m2", content: [{ type: "text", text: "tests too" }] } });
  agent.onMessage(lifecycle("B", "completed"));
  assert.equal(agent.busy, true, "A's turn is still going");
  agent.onMessage({ type: "result", subtype: "success" });
  agent.onMessage(lifecycle("A", "completed"));
  await tick();
  assert.equal(agent.busy, false);
  assert.equal(agent.hello().turnOpen, false);
  assert.deepEqual(JSON.parse(files["/st/s.json"]).open, [], "nothing left open for a restart to send again or call cut off");
  assert.deepEqual(signals, ["busy", "idle"]);
});

test("a prompt whose completion comes after the result settles the busy flag then", async () => {
  // A result that closed nothing (its replies named no prompt): the prompt's
  // own completion is the last word.
  const { agent, signals } = runnerAgent();
  agent.prompt("go", "A");
  agent.onMessage({ type: "assistant", message: { id: "m1", content: [{ type: "text", text: "done" }] } });
  agent.onMessage({ type: "result", subtype: "success" });
  await tick();
  assert.equal(agent.busy, true, "still open by every other sign");
  agent.onMessage(lifecycle("A", "completed"));
  await tick();
  assert.equal(agent.busy, false);
  assert.deepEqual(signals, ["busy", "idle"]);
});

test("a prompt still queued behind a turn keeps the runner busy when another completes", async () => {
  const { agent } = runnerAgent();
  agent.prompt("one", "A");
  agent.prompt("two", "B");
  agent.onMessage({ type: "assistant", user_message_uuid: "A", message: { id: "m1", content: [{ type: "text", text: "1" }] } });
  agent.onMessage({ type: "result", subtype: "success" });
  agent.onMessage(lifecycle("A", "completed"));
  await tick();
  assert.equal(agent.busy, true, "B has not run yet");
  assert.deepEqual(agent.delivered(["B"]), []);
});

test("an agent pane takes no notice of a prompt's lifecycle", async () => {
  const { agent, events, signals } = makeAgent();
  agent.prompt("hi");
  await tick();
  const before = events.length;
  agent.onMessage(lifecycle("x", "started"));
  agent.onMessage(lifecycle("x", "completed"));
  await tick();
  assert.equal(events.length, before);
  assert.deepEqual(signals, ["busy"]);
});

// --- a thread member's fences (--read-only, --keep-out) ---

const denied = (out) => out.hookSpecificOutput && out.hookSpecificOutput.permissionDecision === "deny" ? out.hookSpecificOutput.permissionDecisionReason : "";

test("a read-only member reads on, and its first change asks the daemon for its worktree", async () => {
  const moved = { ok: true, json: async () => ({ message: "moving to /wt/api/thread-3", notice: "[ccmux] You now work in /wt/api/thread-3." }) };
  const { agent, firstEdits, files } = runnerAgent({ readOnly: true, firstEdit: () => moved });
  const [m] = agent.options().hooks.PreToolUse;
  assert.equal(m.matcher, undefined, "every tool is checked: a shell command writes too");
  assert.equal(m.timeout, 120, "long enough for the daemon to make a worktree");
  const hook = m.hooks[0];
  assert.deepEqual(await hook({ tool_name: "Read", tool_input: { file_path: "/inst/a.go" } }), {});
  assert.deepEqual(await hook({ tool_name: "Bash", tool_input: { command: "git status" } }), {});
  assert.deepEqual(firstEdits, [], "reading asks for nothing");
  assert.equal(denied(await hook({ tool_name: "Edit", tool_input: { file_path: "/inst/a.go" } })), "moving to /wt/api/thread-3");
  assert.deepEqual(firstEdits, [{ tool: "Edit" }]);
  assert.equal(JSON.parse(files["/st/s.json"]).note, "[ccmux] You now work in /wt/api/thread-3.", "kept for the start in the worktree");
  assert.equal(denied(await hook({ tool_name: "Bash", tool_input: { command: "npm test" } })), "moving to /wt/api/thread-3", "a command that does not plainly read moves it too");
});

test("a read-only member is refused whatever the daemon says, or fails to say", async () => {
  const refused = { ok: false, status: 500, json: async () => ({ error: "git worktree add: disk full" }) };
  const { agent } = runnerAgent({ readOnly: true, firstEdit: () => refused });
  const hook = agent.options().hooks.PreToolUse[0].hooks[0];
  const why = denied(await hook({ tool_name: "Write", tool_input: { file_path: "/inst/a.go" } }));
  assert.match(why, /only reads in \/inst/);
  assert.match(why, /disk full/);
  const { agent: down } = runnerAgent({ readOnly: true, firstEdit: () => { throw new Error("connect ECONNREFUSED"); } });
  assert.match(denied(await down.options().hooks.PreToolUse[0].hooks[0]({ tool_name: "Write", tool_input: { file_path: "/inst/a.go" } })), /ECONNREFUSED/);
  // A check that throws must refuse: Claude Code runs the tool when a hook throws.
  assert.match(denied(await agent.readOnlyHook({ tool_name: "Write", get tool_input() { throw new Error("odd input"); } })), /could not be checked.*odd input/);
});

test("a member in its worktree is kept out of the repo's own folder, with no word to the daemon", async () => {
  const { agent, firstEdits } = runnerAgent({ keepOut: "/r/api" });
  const hook = agent.options().hooks.PreToolUse[0].hooks[0];
  assert.deepEqual(await hook({ tool_name: "Write", tool_input: { file_path: "/inst/a.go" } }), {});
  assert.match(denied(await hook({ tool_name: "Write", tool_input: { file_path: "/r/api/a.go" } })), /the repo's own folder/);
  assert.deepEqual(firstEdits, []);
  // A task's builder: the same fence for its file tools, none for its shell.
  const { agent: builder } = runnerAgent({ keepOutFiles: "/r/api" });
  const fence = builder.options().hooks.PreToolUse[0].hooks[0];
  assert.match(denied(await fence({ tool_name: "Edit", tool_input: { file_path: "/r/api/a.go" } })), /the repo's own folder/);
  assert.deepEqual(await fence({ tool_name: "Bash", tool_input: { command: "cp /r/api/.env.local ." } }), {});
  const { agent: walled } = makeAgent({ writeRoots: ["/inst"], readOnly: true });
  assert.deepEqual(walled.options().hooks.PreToolUse.map((h) => h.matcher), ["Edit|Write|MultiEdit|NotebookEdit", undefined], "the walls keep their own hook");
});

test("a runner restarted on purpose says what the restart was for, once, and not that a turn was cut off", async () => {
  const state = JSON.stringify({ open: [{ id: "p0", text: "add the endpoint" }], delivered: [], note: "[ccmux] You now work in /wt/api/thread-3." });
  const { agent, sdk, files } = runnerAgent({ keepOut: "/r/api", readFile: (p) => (p === "/st/s.json" ? state : "# base\n") });
  sdk.stored["sess-1"] = [{ type: "user", uuid: "p0", message: { role: "user", content: "add the endpoint" } }];
  await agent.preload();
  agent.recover();
  await new Promise((r) => setImmediate(r));
  const sent = sdk.queries[0].sent.map((m) => m.message.content);
  assert.deepEqual(sent, ["[ccmux] You now work in /wt/api/thread-3."]);
  assert.equal(JSON.parse(files["/st/s.json"]).note, "", "said once");
});

test("a failed turn that touched no cache leaves the previous turn's cache countdown standing", async () => {
  const { agent, events } = runnerAgent();
  agent.ensureQuery();
  agent.onMessage(initMsg());
  agent.onMessage({ type: "assistant", message: { id: "m1", content: [{ type: "text", text: "One." }] } });
  agent.onMessage({ ...resultMsg, usage: { input_tokens: 3, output_tokens: 7, cache_read_input_tokens: 900, cache_creation: { ephemeral_1h_input_tokens: 40 } } });
  await tick();
  const first = events.filter((e) => e.type === "session.idle").pop().properties.result;
  assert.equal(first.cacheTtlMs, 3600000);
  agent.onMessage({ type: "assistant", message: { id: "m2", content: [{ type: "text", text: "Two." }] } });
  agent.onMessage({ type: "result", subtype: "error", is_error: true, result: "overloaded", usage: {} });
  await tick();
  const second = events.filter((e) => e.type === "session.idle").pop().properties.result;
  assert.deepEqual([second.endedAt, second.cacheTtlMs], [first.endedAt, 3600000], "the runner hands the turn before's result on, so nothing restarts the countdown");
});
