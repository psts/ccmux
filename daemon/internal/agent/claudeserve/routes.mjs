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
  const byID = ["session", "permission", "question"].includes(seg[0]) && seg.length > 1;
  const key = method + " " + (byID ? [seg[0], ":id", ...seg.slice(2)] : seg).join("/");
  return { key, id: byID ? seg[1] : "" };
}

// promptText is the text of a prompt_async body: its text parts joined.
export function promptText(body) {
  return (body.parts || []).filter((p) => p && p.type === "text").map((p) => p.text).join("\n");
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
function eventStream(agent, req, res) {
  res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache", connection: "keep-alive" });
  res.write("data: " + JSON.stringify({ type: "server.connected", properties: {} }) + "\n\n");
  agent.clients.add(res);
  req.on("close", () => agent.clients.delete(res));
}

export async function dispatch(agent, req, res) {
  const url = new URL(req.url, "http://127.0.0.1");
  if (req.method === "GET" && url.pathname === "/event") return eventStream(agent, req, res);
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
