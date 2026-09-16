// node --test agent.test.mjs — the agent class against a stub SDK: what a
// permission or question reply resolves to, what an interrupt does, which
// signals the daemon gets, and how a failed query ends.
import { test } from "node:test";
import assert from "node:assert/strict";
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
  const deps = {
    ...sdk, cwd: "/inst", env: { CCMUX_DAEMON_URL: "http://d", CCMUX_PANE_ID: "p1", CCMUX_PANE_TOKEN: "tok-p1", ...(opts.env || {}) },
    readFile: () => "# base\n", out: (s) => out.push(s), err: (s) => sdk.err.push(s),
    fetch: async (url, init) => {
      headers.push(init.headers);
      if (url.endsWith("/agent-ask")) {
        asks.push(JSON.parse(init.body));
        if (opts.askStatus) return { ok: false, status: opts.askStatus, json: async () => ({ error: opts.askStatus === 401 ? "invalid pane token" : "no peer is registered on this pane" }) };
        return { ok: true, json: async () => ({ ok: true, relayed_to: opts.relayedTo ?? 1 }) };
      }
      assert.equal(url, "http://d/v1/panes/p1/agent-signal");
      signals.push(JSON.parse(init.body).state);
      if (opts.signalFails) throw new Error("connect ECONNREFUSED");
      if (opts.signalStatus) return { ok: false, status: opts.signalStatus };
      return { ok: true };
    },
  };
  const agent = new Agent({ agent: "probe", addDirs: [], allowed: [], disallowed: [], ...opts }, deps);
  const events = [];
  agent.clients.add({ write: (line) => events.push(JSON.parse(line.slice(6))), end() {} });
  return { agent, sdk, out, signals, asks, headers, events };
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
  assert.deepEqual(q.options.allowedTools, ["Read"]);
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
  assert.deepEqual(signals, ["needs-input", "needs-input"], "the relay is not a signal");
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
