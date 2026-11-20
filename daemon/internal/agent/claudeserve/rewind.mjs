// Going back to an earlier prompt (a runner's "Edit" on the thread page):
// the files Claude changed since that prompt are put back, and the
// conversation is cut to just before it, as a fork under a new id. The
// daemon then starts the session again on the fork, and the person sends
// the prompt anew, edited. Seen live 2026-11-20 on SDK 0.3.311 with haiku:
// rewindFiles works in a process started after the turns it undoes (before
// any prompt), and a fork up to the entry before a prompt leaves Claude no
// memory of that prompt or anything after it.
//
// What a function here reads from the agent: busy, sid, cwd, rewound, q,
// ensureQuery(), and sdk.getSessionMessages / sdk.forkSession.
import { randomUUID } from "node:crypto";

// interrupted is the line Claude Code writes as a user message when a turn
// is stopped (seen live 2026-11-20): its own, not a prompt.
const interrupted = /^\[Request interrupted by user/;

// refusal is a "no" the person can act on (Claude is working, not a
// prompt): the route answers it as a 409. Anything else is a failure.
export function refusal(text) {
  const err = new Error(text);
  err.refused = true;
  return err;
}

// isPrompt is a stored message a person (or the bus) sent: a user message
// with words or a picture, not a tool's result handed back to Claude, nor
// Claude Code's own line about a stopped turn.
export function isPrompt(m) {
  if (!m || m.type !== "user" || !m.message || m.isMeta) return false;
  const c = m.message.content;
  if (typeof c === "string") return c.trim() !== "" && !interrupted.test(c);
  if (!Array.isArray(c) || c.some((b) => b && b.type === "tool_result")) return false;
  const words = c.filter((b) => b && b.type === "text").map((b) => b.text || "").join("\n").trim();
  return c.some((b) => b && b.type === "image") || (words !== "" && !interrupted.test(words));
}

// forkPoint is where the conversation is cut for going back to prompt
// `to`: the entry just before it (null when `to` opened the conversation,
// so nothing is kept), and how many prompts go, `to` included. {error}
// when `to` is not a prompt of this conversation.
export function forkPoint(msgs, to) {
  const at = msgs.findIndex((m) => m.uuid === to);
  if (at < 0) return { error: "that message is not in this conversation" };
  if (!isPrompt(msgs[at])) return { error: "that message is not a prompt" };
  return { after: at > 0 ? msgs[at - 1].uuid : null, dropped: msgs.slice(at).filter(isPrompt).length };
}

// fileCheck is what undoing the files would do, without doing it. A
// prompt from before checkpoints were kept cannot be undone, and says why.
async function fileCheck(q, to) {
  try {
    const r = await q.rewindFiles(to, { dryRun: true });
    return { canRewind: !!r.canRewind, error: r.error || "", files: r.filesChanged || [] };
  } catch (err) {
    return { canRewind: false, error: err && err.message ? err.message : String(err), files: [] };
  }
}

// rewind looks (dry) or goes back to prompt `to`. Going back undoes the
// files when it can, forks the conversation, and stops this process from
// taking prompts: from here they belong to the fork, which only the next
// start runs, so the daemon keeps them until then. The answer says which
// conversation to start on.
export async function rewind(a, to, dry) {
  if (a.rewound) throw refusal("this session is already going back; it starts again in a moment");
  if (a.busy) throw refusal("Claude is working; stop the turn first");
  if (dry) {
    const { point, files } = await look(a, to);
    return { dropped: point.dropped, files };
  }
  // Held from the start: a prompt let in while this runs would open a turn
  // on the conversation being left.
  a.rewound = true;
  a.rewinding = true; // said as busy in hello: the daemon restarts no runner mid-write
  let restored = false;
  try {
    const { point, files } = await look(a, to);
    if (files.canRewind) {
      const done = await a.q.rewindFiles(to);
      if (!done.canRewind) Object.assign(files, { canRewind: false, error: done.error || "the files could not be put back" });
      restored = !!done.canRewind;
    }
    const conversation = point.after ? (await a.sdk.forkSession(a.sid, { dir: a.cwd, upToMessageId: point.after })).sessionId : randomUUID();
    return { dropped: point.dropped, files, conversation };
  } catch (err) {
    a.rewound = false;
    if (err && err.refused) throw err; // a "no" stays a "no": nothing was changed
    const said = err && err.message ? err.message : String(err);
    throw new Error(restored ? `the files were put back, but the conversation could not be cut: ${said}` : said);
  } finally {
    a.rewinding = false;
  }
}

// look is where the cut falls and what undoing the files would do.
async function look(a, to) {
  const point = forkPoint(await a.sdk.getSessionMessages(a.sid, { dir: a.cwd }), to);
  if (point.error) throw refusal(point.error);
  a.ensureQuery();
  const files = await fileCheck(a.q, to);
  return { point, files };
}
