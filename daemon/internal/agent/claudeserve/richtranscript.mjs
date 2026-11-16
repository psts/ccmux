// The transcript a runner session keeps for the thread page: the agent
// chat's (translate.mjs) plus what that page draws and the chat never
// showed. A file-writing tool carries its diff, a tool result its images, a
// prompt its pasted images, a to-do tool moves the session's to-do list, and
// what Claude Code says beside the conversation (a hook's verdict, a retry)
// is a note in its place. Pure: the image store comes in (media.mjs), so
// richtranscript.test.mjs runs it without a disk.
//
// Only a runner uses it. An agent pane keeps Transcript itself, so the chat
// view and the attention board read exactly what they always did.
import { Transcript } from "./translate.mjs";
import { toolDiff, todosAfter } from "./toolview.mjs";
import { imageRefs } from "./media.mjs";

// What Claude Code writes into a user message for a slash command, as it is
// read back from disk: the command as typed, its output, or a line that is
// only for the model.
const commandName = /<command-name>\s*(\/[^<\s]*)\s*<\/command-name>/;
const commandArgs = /<command-args>([\s\S]*?)<\/command-args>/;
const localOutput = /^\s*<local-command-stdout>([\s\S]*)<\/local-command-stdout>\s*$/;
const forTheModel = /^\s*<local-command-caveat>/;

// promptView reads one user text: {text} as the page shows it, with mark
// {synthetic: true} for one the human did not type; text null hides it.
export function promptView(text, m) {
  if (forTheModel.test(text)) return { text: null };
  const out = localOutput.exec(text);
  if (out) return { text: out[1].trim(), mark: { synthetic: true } };
  const name = commandName.exec(text);
  if (name) {
    const args = commandArgs.exec(text);
    return { text: (name[1] + " " + (args ? args[1].trim() : "")).trim() };
  }
  return m && m.isSynthetic ? { text, mark: { synthetic: true } } : { text };
}

// noteFor maps what Claude Code says beside the conversation onto a note's
// text, "" for a message that is not shown. One entry per system subtype.
const notes = {
  informational: (m) => String(m.content || ""),
  api_retry: (m) => `The API did not answer (${m.error_status || m.error || "no response"}). Trying again, attempt ${m.attempt} of ${m.max_retries}.`,
  compact_boundary: () => "The conversation was compacted to make room.",
};

export function noteFor(m) {
  const say = m && m.type === "system" ? notes[m.subtype] : null;
  return say ? say(m) : "";
}

export class RichTranscript extends Transcript {
  // media is the session's image store, or null to keep no images.
  constructor(sessionID, now, media = null) {
    super(sessionID, now);
    this.media = media;
    this.todos = [];
    this.todosMoved = false;
  }

  // A file-writing call shows what it is about to change as soon as its
  // input is whole: a permission card for it is up before any result.
  toolFilled(part) {
    const diff = toolDiff(part.tool, part.state.input);
    if (diff) part.state.diff = diff;
  }

  // A result settles the diff (Claude Code's own patch, when the SDK hands
  // it over), keeps the images the tool returned, and moves the to-do list.
  toolSettled(part, block, m) {
    if (block.is_error) {
      delete part.state.diff; // nothing was written
      return;
    }
    const diff = toolDiff(part.tool, part.state.input, m.tool_use_result);
    if (diff) part.state.diff = diff;
    const images = imageRefs(this.media, block.content);
    if (images.length) part.state.images = images;
    const todos = todosAfter(this.todos, part.tool, part.state.input, m.tool_use_result);
    if (todos) {
      this.todos = todos;
      this.todosMoved = true;
    }
  }

  userPrompt(texts, m) {
    const view = promptView(texts.join("\n"), m);
    return { ...view, files: imageRefs(this.media, m.message.content) };
  }

  settled() {
    if (!this.todosMoved) return [];
    this.todosMoved = false;
    return [{ type: "session.todos", properties: { sessionID: this.sessionID, todos: this.todos } }];
  }

  // A note is beside the conversation, not a turn of it: the reply being
  // written is still the newest turn that is not a note. Without this a
  // note that arrives mid-turn (an API retry, a hook's verdict) would take
  // the reply's place as "the current turn", and a failure that follows
  // would land on nothing and never reach the conversation.
  currentAssistant() {
    for (let i = this.turns.length - 1; i >= 0; i--) {
      const turn = this.turns[i];
      if (turn.info.role !== "note") return turn.info.role === "assistant" ? turn : null;
    }
    return null;
  }

  // note puts a line Claude Code said beside the conversation into it, as a
  // turn of its own (role "note"), so the page shows it where it happened.
  // level is Claude Code's: info, notice, suggestion or warning.
  note(text, level = "notice") {
    if (!text) return [];
    const turn = this.ensureTurn("note_" + this.nextID(), "note");
    turn.info.level = level;
    return this.userTurn(text, turn.info.id);
  }
}
