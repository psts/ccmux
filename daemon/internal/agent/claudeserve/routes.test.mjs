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

test("a body that is not JSON is refused with 400, and the card stays pending", async () => {
  await withServer(async (agent, call, base) => {
    const r = await fetch(base + "/permission/per_1/reply", { method: "POST", headers: { "content-type": "application/json" }, body: "{bad" });
    assert.equal(r.status, 400);
    assert.match((await r.json()).error, /not JSON/);
    assert.deepEqual(agent.replies, {}, "no reply reached the agent");
    assert.equal(agent.logged.length, 1);
    const ok = await call("POST", "/permission/per_1/reply", { reply: "once" });
    assert.equal(ok.status, 200, "a valid body afterwards still works");
    const empty = await fetch(base + "/tui/submit-prompt", { method: "POST" });
    assert.equal(empty.status, 200, "an empty body is still an empty object");
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

test("the runner routes: hello, prompts by inbox id, delivered, and a replay after n", async () => {
  await withServer(async (agent, call, base) => {
    const { EventLog } = await import("./runnerstate.mjs");
    agent.events = new EventLog();
    agent.events.push({ type: "a" });
    agent.events.push({ type: "b" });
    agent.hello = () => ({ protocol: 1, session: "ses_1" });
    agent.prompt = (t, id, images) => { agent.prompts.push([t, id]); agent.images = images; return id !== "dup"; };
    agent.delivered = (ids) => ids.filter((id) => id === "p1");
    assert.equal((await call("GET", "/hello")).body.protocol, 1);
    assert.deepEqual((await call("POST", "/prompt", { id: "p1", text: "hi" })).body, { sent: true });
    assert.deepEqual((await call("POST", "/prompt", { id: "dup", text: "hi" })).body, { sent: false });
    assert.equal((await call("POST", "/prompt", { text: "hi" })).status, 400, "a prompt needs its inbox id");
    assert.equal((await call("POST", "/prompt", { id: "p2", text: " " })).status, 400);
    assert.deepEqual((await call("POST", "/prompt", { id: "p3", text: "", images: ["a.png"] })).body, { sent: true }, "a picture with no words is a prompt");
    assert.equal((await call("POST", "/prompt", { id: "p4", text: " ", images: [] })).status, 400);
    assert.equal((await call("POST", "/model", { model: "haiku" })).status, 404, "no live model switch: it applies at the next start");
    assert.deepEqual(agent.images, ["a.png"], "the names go to the agent with the prompt");
    assert.deepEqual((await call("POST", "/delivered", { ids: ["p1", "p9"] })).body, { delivered: ["p1"] });
    const ctl = new AbortController();
    const r = await fetch(base + "/event?after=1", { signal: ctl.signal });
    const reader = r.body.getReader();
    let text = "";
    while (!text.includes('"type":"b"')) text += new TextDecoder().decode((await reader.read()).value);
    ctl.abort();
    assert.match(text, /"gap":false/);
    assert.match(text, /id: 2\ndata: \{"type":"b"\}/);
    assert.doesNotMatch(text, /"type":"a"/, "only events after 1");
  });
});

test("the snapshot route, and the event stream naming its process", async () => {
  await withServer(async (agent, call, base) => {
    const { EventLog } = await import("./runnerstate.mjs");
    agent.events = new EventLog();
    agent.events.push({ type: "a" });
    agent.boot = "boot-7";
    agent.snapshot = () => ({ boot: agent.boot, seq: agent.events.seq, messages: [{ info: { id: "m1" }, parts: [] }] });
    assert.deepEqual((await call("GET", "/snapshot")).body, { boot: "boot-7", seq: 1, messages: [{ info: { id: "m1" }, parts: [] }] });
    const ctl = new AbortController();
    const r = await fetch(base + "/event?after=1", { signal: ctl.signal });
    const first = new TextDecoder().decode((await r.body.getReader().read()).value);
    ctl.abort();
    assert.match(first, /"type":"server.connected","properties":\{"gap":false,"seq":1,"boot":"boot-7"\}/);
  });
});

test("the rewind route: a look or a going back, a refusal is a 409 in the agent's words, a failure a logged 500", async () => {
  await withServer(async (agent, call) => {
    const asked = [];
    agent.rewind = async (to, dry) => {
      asked.push([to, dry]);
      if (to === "busy") throw Object.assign(new Error("Claude is working; stop the turn first"), { refused: true });
      if (to === "disk") throw new Error("the files were put back, but the conversation could not be cut: disk full");
      return { dropped: 1, conversation: dry ? undefined : "f1" };
    };
    assert.deepEqual((await call("POST", "/rewind", { to: "p2", dry: true })).body, { dropped: 1 });
    assert.deepEqual((await call("POST", "/rewind", { to: "p2" })).body, { dropped: 1, conversation: "f1" });
    const busy = await call("POST", "/rewind", { to: "busy" });
    assert.deepEqual([busy.status, busy.body.error], [409, "Claude is working; stop the turn first"]);
    const disk = await call("POST", "/rewind", { to: "disk" });
    assert.equal(disk.status, 500, "a failure is not a refusal");
    assert.ok(agent.logged.some((l) => l.includes("going back failed: the files were put back")), "and it is logged");
    assert.equal((await call("POST", "/rewind", {})).status, 400);
    assert.deepEqual(asked, [["p2", true], ["p2", false], ["busy", false], ["disk", false]]);
  });
});

test("the mode, effort and task-stop routes: done is a 200, a no is a 409 in the agent's words, a failure a logged 500", async () => {
  await withServer(async (agent, call) => {
    const did = [];
    agent.setMode = async (mode) => { if (mode === "nope") throw Object.assign(new Error("not a mode to switch to: nope"), { refused: true }); did.push(["mode", mode]); };
    agent.stopTask = async (id) => { if (id === "gone") throw new Error("Claude Code exited"); did.push(["stop", id]); };
    assert.equal((await call("POST", "/mode", { mode: "plan" })).status, 200);
    const no = await call("POST", "/mode", { mode: "nope" });
    assert.deepEqual([no.status, no.body.error], [409, "not a mode to switch to: nope"]);
    assert.equal((await call("POST", "/task/b1/stop", {})).status, 200);
    const broke = await call("POST", "/task/gone/stop", {});
    assert.equal(broke.status, 500, "a failure is not a refusal");
    assert.ok(agent.logged.some((l) => l.includes("Claude Code exited")), "and it is logged");
    agent.setEffort = async (level) => { did.push(["effort", level]); };
    assert.equal((await call("POST", "/effort", { effort: "max" })).status, 200);
    assert.equal((await call("POST", "/effort", {})).status, 200, "none: the model's own");
    assert.deepEqual(did, [["mode", "plan"], ["stop", "b1"], ["effort", "max"], ["effort", ""]]);
  });
});
