// The sidecar's HTTP surface: the opencode session-API subset the daemon
// speaks (agent/opencodeclient.go, agent/opencodepush.go), as a route table
// over a small agent interface. No SDK import, so it is tested on its own
// with a stub agent (routes.test.mjs).
//
// What a handler reads from the agent: sid, pending (text appended by the
// TUI pair), clients (event-stream responses), log(line), and the methods
// sessions(), messages(id), prompt(text), interrupt(), pendingPermissions(),
// replyPermission(id, reply), pendingQuestions(), replyQuestion(id, answers),
// rejectQuestion(id).
import { createServer } from "node:http";

// routeKey maps a request onto a table key: session/{id}/..., permission/
// {id}/... and question/{id}/... carry an id in the second segment, every
// other path is matched literally.
export function routeKey(method, pathname) {
  const seg = pathname.split("/").filter(Boolean);
  const byID = ["session", "permission", "question", "task"].includes(seg[0]) && seg.length > 1;
  const key = method + " " + (byID ? [seg[0], ":id", ...seg.slice(2)] : seg).join("/");
  return { key, id: byID ? seg[1] : "" };
}

// promptText is the text of a prompt_async body: its text parts joined.
export function promptText(body) {
  return (body.parts || []).filter((p) => p && p.type === "text").map((p) => p.text).join("\n");
}

// answered runs one action for the page: 200 when it took, 409 with the
// reason when the agent said no (a refusal, rewind.mjs), and anything else
// (the SDK failed, Claude Code is gone) thrown on, for the server to log
// and answer as a 500.
async function answered(work) {
  try {
    await work();
    return [200, {}];
  } catch (err) {
    if (err && err.refused) return [409, { error: err.message }];
    throw err;
  }
}

// Each handler returns [status, body]; the body is sent as JSON.
export const handlers = {
  "GET session": async (a) => [200, await a.sessions()],
  "GET session/:id/message": async (a, id) => [200, await a.messages(id)],
  "POST session/:id/prompt_async": (a, id, body) => {
    if (id !== a.sid) return [400, { error: "not the current session" }];
    const text = promptText(body);
    if (!text.trim()) return [400, { error: "empty prompt" }];
    a.prompt(text);
    return [200, {}];
  },
  "POST session/:id/abort": async (a) => {
    await a.interrupt();
    return [200, {}];
  },
  "GET permission": (a) => [200, a.pendingPermissions()],
  "POST permission/:id/reply": (a, id, body) => [a.replyPermission(id, body.reply) ? 200 : 404, {}],
  "GET question": (a) => [200, a.pendingQuestions()],
  "POST question/:id/reply": (a, id, body) => [a.replyQuestion(id, body.answers) ? 200 : 404, {}],
  "POST question/:id/reject": (a, id) => [a.rejectQuestion(id) ? 200 : 404, {}],
  // A session runner's own surface (the daemon's internal/runner): its
  // versions at connect, prompts by inbox id, model switches, and which
  // prompts Claude already has.
  "GET hello": (a) => [200, a.hello()],
  // The conversation and where the event log stands, in one answer, for a
  // reader that then follows /event?after=<seq> (the thread page's relay).
  "GET snapshot": (a) => [200, a.snapshot()],
  "POST prompt": (a, id, body) => {
    const text = String(body.text || "");
    // images are names in the session's media folder; a picture with no
    // words is still a prompt.
    const images = Array.isArray(body.images) ? body.images.map(String) : [];
    if (!text.trim() && !images.length) return [400, { error: "empty prompt" }];
    if (!body.id) return [400, { error: "a prompt needs its inbox id" }];
    return [200, { sent: a.prompt(text, String(body.id), images) }];
  },
  // Going back to an earlier prompt (rewind.mjs): dry says what it would
  // do; otherwise it is done, and the answer names the conversation the
  // session starts again on. A refusal is a 409; a failure is logged and a
  // 500.
  "POST rewind": async (a, id, body) => {
    if (!body.to) return [400, { error: "rewind needs the prompt to go back to" }];
    try {
      return [200, await a.rewind(String(body.to), !!body.dry)];
    } catch (err) {
      const text = err && err.message ? err.message : String(err);
      if (err && err.refused) return [409, { error: text }];
      a.log("! going back failed: " + text);
      return [500, { error: text }];
    }
  },
  // A running session's permission mode, switched from its page, and one
  // of its background tasks stopped. A "no" is a 409 in the agent's words.
  "POST mode": (a, id, body) => answered(() => a.setMode(String(body.mode || ""))),
  "POST task/:id/stop": (a, id) => answered(() => a.stopTask(id)),
  "POST delivered": (a, id, body) => [200, { delivered: a.delivered(Array.isArray(body.ids) ? body.ids : []) }],
  // The daemon's PushPrompt (first message, bus messages) speaks opencode's
  // TUI pair: append text to the prompt box, then submit it.
  "POST tui/append-prompt": (a, id, body) => {
    a.pending += String(body.text || "");
    return [200, true];
  },
  "POST tui/submit-prompt": (a) => {
    const text = a.pending.trim();
    a.pending = "";
    if (text) a.prompt(text);
    return [200, true];
  },
};

// readJSON is the request body as JSON. A body that is not JSON rejects,
// its only rejection: read as empty it would turn a permission reply into
// a silent deny.
export function readJSON(req) {
  return new Promise((resolve, reject) => {
    let body = "";
    // One decoder across chunks: a multi-byte character split at a chunk
    // boundary would otherwise decode to U+FFFD on both sides.
    req.setEncoding("utf8");
    req.on("data", (c) => { body += c; });
    req.on("end", () => {
      try { resolve(body ? JSON.parse(body) : {}); } catch (err) { reject(new Error("body is not JSON: " + err.message)); }
    });
  });
}

export function send(res, status, body) {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(body === undefined ? {} : body));
}

// eventStream is GET /event: server-sent events, one JSON object per data
// line, kept open until the client leaves. Whoever broadcasts writes to
// every response in agent.clients.
// A runner's reader passes ?after=N to get the numbered events it missed
// first (and gap: true in the connected event when some were dropped). The
// connected event names this process too (boot), so a reader that asked for
// events after N of an earlier process sees it is talking to another one.
function eventStream(agent, req, res, after) {
  res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache", connection: "keep-alive" });
  const replay = after == null || !agent.events ? null : agent.events.after(after);
  const props = replay ? { gap: replay.gap, seq: agent.events.seq, boot: agent.boot } : {};
  res.write("data: " + JSON.stringify({ type: "server.connected", properties: props }) + "\n\n");
  for (const x of replay ? replay.events : []) res.write("id: " + x.seq + "\ndata: " + JSON.stringify(x.ev) + "\n\n");
  agent.clients.add(res);
  req.on("close", () => agent.clients.delete(res));
}

export async function dispatch(agent, req, res) {
  const url = new URL(req.url, "http://127.0.0.1");
  if (req.method === "GET" && url.pathname === "/event") return eventStream(agent, req, res, url.searchParams.get("after"));
  const { key, id } = routeKey(req.method, url.pathname);
  const handler = handlers[key];
  if (!handler) return send(res, 404, { error: "no such route" });
  let body = {};
  if (req.method === "POST") {
    try {
      body = await readJSON(req);
    } catch (err) {
      agent.log("! " + err.message);
      return send(res, 400, { error: err.message });
    }
  }
  const [status, out] = await handler(agent, id, body);
  return send(res, status, out);
}

// createAgentServer is the loopback server over one agent; an error from a
// handler is answered as 500 and said in the agent's log.
export function createAgentServer(agent) {
  return createServer((req, res) => {
    dispatch(agent, req, res).catch((err) => {
      const text = err && err.message ? err.message : String(err);
      agent.log("! " + text);
      if (!res.headersSent) send(res, 500, { error: text });
    });
  });
}
