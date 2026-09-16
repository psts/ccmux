// node --test agent.test.mjs — the agent class against a stub SDK: what a
// permission or question reply resolves to, what an interrupt does, which
// signals the daemon gets, and how a failed query ends.
import { test } from "node:test";
import assert from "node:assert/strict";
import { Agent } from "./agent.mjs";

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
  const deps = {
    ...sdk, cwd: "/inst", env: { CCMUX_DAEMON_URL: "http://d", CCMUX_PANE_ID: "p1", ...(opts.env || {}) },
    readFile: () => "# base\n", out: (s) => out.push(s), err: (s) => sdk.err.push(s),
    fetch: async (url, init) => { signals.push(JSON.parse(init.body).state); return { ok: true }; },
  };
  const agent = new Agent({ agent: "probe", addDirs: [], allowed: [], disallowed: [], ...opts }, deps);
  const events = [];
  agent.clients.add({ write: (line) => events.push(JSON.parse(line.slice(6))), end() {} });
  return { agent, sdk, out, signals, events };
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
