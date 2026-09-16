// Translation from Claude Agent SDK messages to the session shapes the
// daemon reads from a running agent (the opencode session-API subset in
// agent/opencodeclient.go): a conversation is a list of {info, parts}, and a
// live change is an event {type, properties}. Pure: no I/O, no SDK import,
// so it can be tested with node --test and read on its own.
//
// Parts keep the wire names the daemon's normalizer expects: "text",
// "reasoning" (thinking), and "tool" with a state {status, input, output,
// title, error}. A user prompt is one text part; a tool result lands on the
// tool part it answers, found by its call id.

// sessionInfo is one on-disk conversation as the daemon lists sessions:
// its title is the custom title, else the SDK's summary, else the first
// prompt's first line; its directory is the folder it was opened in.
export function sessionInfo(s) {
  return {
    id: s.sessionId,
    title: s.customTitle || s.summary || firstLine(s.firstPrompt),
    directory: s.cwd || "",
    time: { created: s.createdAt || 0, updated: s.lastModified || 0 },
  };
}

function firstLine(text) {
  return String(text || "").split("\n")[0].slice(0, 80);
}

// toolTitle is the one-line summary of a tool call the chat shows beside
// the tool name, from the fields a human recognizes.
export function toolTitle(name, input) {
  if (!input || typeof input !== "object") return name;
  const pick = input.command || input.description || input.file_path || input.pattern || input.url || input.prompt || input.query;
  return pick ? firstLine(String(pick)) : name;
}

// permissionPattern is what the permission card prints after the tool
// name: the command, the path, or a compact view of the input.
export function permissionPattern(name, input) {
  const title = toolTitle(name, input);
  if (title !== name) return title;
  const json = JSON.stringify(input || {});
  return json.length > 120 ? json.slice(0, 117) + "..." : json;
}

// questionRequest maps the AskUserQuestion tool's input onto the question
// request shape (one or more questions, each with options).
export function questionRequest(id, sessionID, input, callID) {
  const questions = (input?.questions || []).map((q) => ({
    question: String(q.question || ""),
    header: String(q.header || ""),
    options: (q.options || []).map((o) => ({ label: String(o.label || ""), description: String(o.description || "") })),
    multiple: !!q.multiSelect,
    custom: true,
  }));
  return { id, sessionID, questions, tool: { messageID: "", callID: callID || "" } };
}

// questionAnswers turns the chat's reply (one list of labels per question)
// into the answers map the tool expects back in its input.
export function questionAnswers(input, answers) {
  const out = {};
  (input?.questions || []).forEach((q, i) => {
    const picked = Array.isArray(answers?.[i]) ? answers[i] : [];
    if (picked.length === 0) return;
    out[q.question] = q.multiSelect ? picked : picked[0];
  });
  return out;
}

// resultText flattens a tool result's content (a string, or text blocks).
export function resultText(content) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content.map((b) => (b && b.type === "text" ? b.text : "")).filter(Boolean).join("\n");
}

// Transcript is one session's conversation, kept in the daemon's shape and
// updated from SDK messages; every update returns the events to broadcast.
export class Transcript {
  constructor(sessionID, now = () => Date.now()) {
    this.sessionID = sessionID;
    this.now = now;
    this.turns = [];
    this.byID = new Map(); // message id → turn
    this.byCall = new Map(); // tool call id → part
    this.streamed = new Map(); // message id → parts by block index
    this.seq = 0;
  }

  // messages is the whole conversation in GET /session/{id}/message shape.
  messages() {
    return this.turns.map((t) => ({ info: t.info, parts: t.parts.map(publicPart) }));
  }

  // userTurn appends a prompt as a user turn; the events announce it.
  userTurn(text, id) {
    const turn = this.ensureTurn(id || "usr_" + this.nextID(), "user");
    const part = { id: turn.info.id + ".0", messageID: turn.info.id, sessionID: this.sessionID, type: "text", text };
    turn.parts.push(part);
    return [messageEvent(turn), partEvent(part)];
  }

  // apply folds one SDK message in and returns the events it caused. A
  // result message is not folded in here: Agent.onResult ends the turn.
  apply(m) {
    switch (m.type) {
      case "stream_event":
        return this.applyStream(m.event);
      case "assistant":
        return this.applyAssistant(m.message, m.error);
      case "user":
        return this.applyUser(m);
      default:
        return [];
    }
  }

  applyStream(ev) {
    switch (ev.type) {
      case "message_start": {
        const turn = this.ensureTurn(ev.message.id, "assistant");
        return [messageEvent(turn)];
      }
      case "content_block_start":
        return this.startBlock(ev);
      case "content_block_delta":
        return this.deltaBlock(ev);
      case "content_block_stop":
        return this.stopBlock(ev);
      default:
        return [];
    }
  }

  // currentAssistant is the message the stream is filling: the newest turn.
  currentAssistant() {
    const t = this.turns[this.turns.length - 1];
    return t && t.info.role === "assistant" ? t : null;
  }

  startBlock(ev) {
    const turn = this.currentAssistant();
    if (!turn) return [];
    const block = ev.content_block || {};
    const part = this.newPart(turn, block.type, block);
    if (!part) return [];
    this.streamed.get(turn.info.id).set(ev.index, part);
    turn.parts.push(part);
    return [partEvent(part)];
  }

  deltaBlock(ev) {
    const turn = this.currentAssistant();
    const part = turn && this.streamed.get(turn.info.id).get(ev.index);
    if (!part) return [];
    const d = ev.delta || {};
    if (d.type === "text_delta" || d.type === "thinking_delta") {
      const text = d.type === "text_delta" ? d.text : d.thinking;
      if (!text) return [];
      part.text += text;
      return [{ type: "message.part.delta", properties: { sessionID: this.sessionID, messageID: part.messageID, partID: part.id, field: "text", delta: text } }];
    }
    if (d.type === "input_json_delta") part.json = (part.json || "") + (d.partial_json || "");
    return [];
  }

  stopBlock(ev) {
    const turn = this.currentAssistant();
    const part = turn && this.streamed.get(turn.info.id).get(ev.index);
    if (!part) return [];
    if (part.type === "tool" && part.json) {
      try {
        part.state.input = JSON.parse(part.json);
        part.state.title = toolTitle(part.tool, part.state.input);
      } catch (_) {
        // A partial tool input that never parsed stays as it was; the
        // complete assistant message that follows carries the real one.
      }
      delete part.json;
    }
    return [partEvent(part)];
  }

  // applyAssistant is the complete message: authoritative for every block,
  // matched to what streamed by call id or by order within its kind.
  applyAssistant(msg, error) {
    const turn = this.ensureTurn(msg.id, "assistant");
    const events = [];
    const seen = new Map();
    for (const block of msg.content || []) {
      const nth = seen.get(block.type) || 0;
      seen.set(block.type, nth + 1);
      let part = block.type === "tool_use" ? this.byCall.get(block.id) : nthOfKind(turn, kindOf(block.type), nth);
      if (!part) {
        part = this.newPart(turn, block.type, block);
        if (!part) continue;
        turn.parts.push(part);
      }
      fillPart(part, block);
      events.push(partEvent(part));
    }
    if (error && !turn.info.error) {
      turn.info.error = { name: String(error), data: { message: "" } };
      events.push(messageEvent(turn));
    }
    return events;
  }

  applyUser(m) {
    const content = m.message?.content;
    if (typeof content === "string") return this.userTurn(content, m.uuid);
    if (!Array.isArray(content)) return [];
    const events = [];
    const texts = [];
    for (const block of content) {
      if (block.type === "tool_result") {
        const part = this.byCall.get(block.tool_use_id);
        if (!part) continue;
        part.state.status = block.is_error ? "error" : "completed";
        const text = resultText(block.content);
        if (block.is_error) part.state.error = text;
        else part.state.output = text;
        events.push(partEvent(part));
      } else if (block.type === "text" && block.text) {
        texts.push(block.text);
      }
    }
    if (texts.length) events.push(...this.userTurn(texts.join("\n"), m.uuid));
    return events;
  }

  // finishTurn marks the turn's end: running tools that never reported are
  // left as they are; an error lands on the last assistant turn.
  finishTurn(error) {
    const turn = this.currentAssistant();
    if (!error || !turn || turn.info.error) return [];
    turn.info.error = { name: "error", data: { message: error } };
    return [messageEvent(turn)];
  }

  ensureTurn(id, role) {
    let turn = this.byID.get(id);
    if (turn) return turn;
    turn = { info: { id, sessionID: this.sessionID, role, time: { created: this.now() } }, parts: [] };
    this.turns.push(turn);
    this.byID.set(id, turn);
    this.streamed.set(id, new Map());
    return turn;
  }

  newPart(turn, blockType, block) {
    const kind = kindOf(blockType);
    if (!kind) return null;
    const part = { id: turn.info.id + "." + turn.parts.length, messageID: turn.info.id, sessionID: this.sessionID, type: kind };
    if (kind === "tool") {
      part.tool = block.name || "";
      part.callID = block.id || "";
      part.state = { status: "running", input: block.input || {}, output: "", title: toolTitle(part.tool, block.input) };
      if (part.callID) this.byCall.set(part.callID, part);
    } else {
      part.text = "";
    }
    return part;
  }

  nextID() {
    return String(++this.seq) + "-" + this.now().toString(36);
  }
}

function kindOf(blockType) {
  switch (blockType) {
    case "text":
      return "text";
    case "thinking":
    case "redacted_thinking":
      return "reasoning";
    case "tool_use":
      return "tool";
    default:
      return null;
  }
}

function nthOfKind(turn, kind, n) {
  let i = 0;
  for (const p of turn.parts) {
    if (p.type !== kind) continue;
    if (i === n) return p;
    i++;
  }
  return null;
}

function fillPart(part, block) {
  switch (block.type) {
    case "text":
      part.text = block.text || "";
      break;
    case "thinking":
      part.text = block.thinking || "";
      break;
    case "tool_use":
      part.state.input = block.input || {};
      part.state.title = toolTitle(part.tool, block.input);
      break;
    default:
      break;
  }
}

function publicPart(p) {
  const { json, ...rest } = p;
  return rest;
}

function messageEvent(turn) {
  return { type: "message.updated", properties: { sessionID: turn.info.sessionID, info: turn.info } };
}

function partEvent(part) {
  return { type: "message.part.updated", properties: { sessionID: part.sessionID, part: publicPart(part) } };
}
