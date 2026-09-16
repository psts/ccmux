// node --test routes.test.mjs — drives the HTTP surface over loopback with a
// stub agent, the way the daemon's opencode client does.
import { test } from "node:test";
import assert from "node:assert/strict";
import { connect } from "node:net";
import { createAgentServer, routeKey, promptText } from "./routes.mjs";

function stubAgent() {
  const a = {
    sid: "ses_1", pending: "", clients: new Set(), logged: [], prompts: [], interrupts: 0, replies: {}, answers: {},
    log: (l) => a.logged.push(l),
    sessions: async () => [{ id: "ses_1", title: "t", directory: "/d", time: { updated: 1 } }],
    messages: async (id) => (id === "ses_1" ? [{ info: { id: "m1", role: "user" }, parts: [] }] : []),
    prompt: (t) => a.prompts.push(t),
    interrupt: async () => { a.interrupts++; },
    pendingPermissions: () => [{ id: "per_1", sessionID: "ses_1", permission: "Bash", patterns: ["ls"] }],
    replyPermission: (id, reply) => { if (id !== "per_1") return false; a.replies[id] = reply; return true; },
    pendingQuestions: () => [{ id: "que_1", sessionID: "ses_1", questions: [] }],
    replyQuestion: (id, answers) => { if (id !== "que_1") return false; a.answers[id] = answers; return true; },
    rejectQuestion: (id) => { if (id !== "que_1") return false; a.answers[id] = null; return true; },
  };
  return a;
}

async function withServer(fn) {
  const agent = stubAgent();
  const server = createAgentServer(agent);
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const base = `http://127.0.0.1:${server.address().port}`;
  const call = async (method, path, body) => {
    const r = await fetch(base + path, { method, headers: { "content-type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });
    return { status: r.status, body: await r.json() };
  };
  try {
    await fn(agent, call, base);
  } finally {
    server.close();
  }
}

test("route keys: ids only in the second segment of session, permission and question", () => {
  assert.deepEqual(routeKey("POST", "/session/ses_1/prompt_async"), { key: "POST session/:id/prompt_async", id: "ses_1" });
  assert.deepEqual(routeKey("POST", "/tui/append-prompt"), { key: "POST tui/append-prompt", id: "" });
  assert.deepEqual(routeKey("GET", "/session"), { key: "GET session", id: "" });
  assert.equal(promptText({ parts: [{ type: "text", text: "a" }, { type: "file" }, { type: "text", text: "b" }] }), "a\nb");
});

test("the session routes: list, messages, prompt for the current session only, abort", async () => {
  await withServer(async (agent, call) => {
    assert.equal((await call("GET", "/session")).body[0].id, "ses_1");
    assert.equal((await call("GET", "/session/ses_1/message")).body.length, 1);
    assert.equal((await call("POST", "/session/ses_1/prompt_async", { parts: [{ type: "text", text: "hi" }] })).status, 200);
    assert.equal((await call("POST", "/session/other/prompt_async", { parts: [{ type: "text", text: "hi" }] })).status, 400);
    assert.equal((await call("POST", "/session/ses_1/prompt_async", { parts: [] })).status, 400);
    assert.deepEqual(agent.prompts, ["hi"]);
    assert.equal((await call("POST", "/session/ses_1/abort", {})).status, 200);
    assert.equal(agent.interrupts, 1);
    assert.equal((await call("GET", "/nope")).status, 404);
  });
});

test("the TUI pair appends then submits as one prompt; an empty submit sends nothing", async () => {
  await withServer(async (agent, call) => {
    await call("POST", "/tui/append-prompt", { text: "first " });
    await call("POST", "/tui/append-prompt", { text: "second" });
    assert.equal((await call("POST", "/tui/submit-prompt", {})).body, true);
    assert.deepEqual(agent.prompts, ["first second"]);
    await call("POST", "/tui/submit-prompt", {});
    assert.equal(agent.prompts.length, 1);
    assert.equal(agent.pending, "");
  });
});

test("permission and question requests: listed, replied, unknown ids are 404", async () => {
  await withServer(async (agent, call) => {
    assert.equal((await call("GET", "/permission")).body[0].id, "per_1");
    assert.equal((await call("POST", "/permission/per_1/reply", { reply: "always" })).status, 200);
    assert.equal((await call("POST", "/permission/per_9/reply", { reply: "once" })).status, 404);
    assert.equal(agent.replies.per_1, "always");
    assert.equal((await call("GET", "/question")).body[0].id, "que_1");
    assert.equal((await call("POST", "/question/que_1/reply", { answers: [["Tea"]] })).status, 200);
    assert.deepEqual(agent.answers.que_1, [["Tea"]]);
    assert.equal((await call("POST", "/question/que_1/reject", {})).status, 200);
    assert.equal(agent.answers.que_1, null);
    assert.equal((await call("POST", "/question/que_9/reject", {})).status, 404);
  });
});

test("the event stream opens with server.connected and receives broadcasts", async () => {
  await withServer(async (agent, call, base) => {
    const ctrl = new AbortController();
    const r = await fetch(base + "/event", { signal: ctrl.signal });
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    let got = dec.decode((await reader.read()).value);
    assert.match(got, /server\.connected/);
    assert.equal(agent.clients.size, 1);
    for (const res of agent.clients) res.write("data: {\"type\":\"session.idle\",\"properties\":{}}\n\n");
    got = dec.decode((await reader.read()).value);
    assert.match(got, /session\.idle/);
    ctrl.abort();
    await new Promise((r2) => setTimeout(r2, 50));
    assert.equal(agent.clients.size, 0, "a client that left is forgotten");
  });
});

test("a body split mid-character across two writes arrives intact", async () => {
  await withServer(async (agent, call, base) => {
    const text = "x".repeat(2000) + "åäö — 🙂";
    const payload = Buffer.from(JSON.stringify({ text }));
    // Cut inside the multi-byte tail: the first write ends one byte into "ä".
    const cut = payload.indexOf(Buffer.from("ä")) + 1;
    const port = Number(new URL(base).port);
    await new Promise((resolve, reject) => {
      const sock = connect(port, "127.0.0.1", () => {
        sock.write(`POST /tui/append-prompt HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: ${payload.length}\r\nConnection: close\r\n\r\n`);
        sock.write(payload.subarray(0, cut));
        setTimeout(() => sock.write(payload.subarray(cut)), 30);
      });
      let reply = "";
      sock.on("data", (d) => { reply += d; });
      sock.on("close", () => (reply.startsWith("HTTP/1.1 200") ? resolve() : reject(new Error(reply.slice(0, 80)))));
      sock.on("error", reject);
    });
    await call("POST", "/tui/submit-prompt", {});
    assert.deepEqual(agent.prompts, [text]);
    assert.ok(!agent.prompts[0].includes("\uFFFD"));
  });
});

test("a handler that throws answers 500 and is logged", async () => {
  await withServer(async (agent, call) => {
    agent.sessions = async () => { throw new Error("store gone"); };
    const r = await call("GET", "/session");
    assert.equal(r.status, 500);
    assert.equal(r.body.error, "store gone");
    assert.deepEqual(agent.logged, ["! store gone"]);
  });
});
