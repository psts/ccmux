// threadpending.js: the messages the human sent that the conversation does
// not hold yet. A prompt reaches the stream only once the session's runner
// has it, which for a new or sleeping session is after its process starts,
// and for a busy one may be after the turn in progress. Until then the page
// draws the message itself, at the end, so what was typed shows the moment
// it is sent.
//
// Each one is {key, sel, where, state, id, error, text, urls, images}: sel
// is what it was sent in and where that one's address (route.js's
// threadHref); state is "new" (not sent yet), "queued" (the daemon has it,
// as prompt id, which becomes the user turn's id in the stream) or
// "failed" (error says why; it waits for Retry or Remove). urls are the
// local pictures, images the files behind them. Plain data
// (threadpending_test.js); threads.js owns the list and does the sending.
(function (root) {
  "use strict";

  // arrived splits the list into the ones the stream does not hold yet and
  // the ones it now does (their user turn is there, by the prompt's id).
  function arrived(list, turns) {
    const have = new Set((turns || []).filter((t) => t.info.role === "user").map((t) => t.info.id));
    const left = [], landed = [];
    for (const p of list) (p.id && have.has(p.id) ? landed : left).push(p);
    return { left, landed };
  }

  // held is whether p waits behind a message of its conversation that
  // failed: Claude gets a conversation's messages in the order they were
  // sent, so nothing after a failed one goes until it is retried or removed.
  function held(list, p) {
    const i = list.indexOf(p);
    return list.slice(0, i).some((q) => q.where === p.where && q.state === "failed");
  }

  // next is the message to send now: the oldest not sent yet and not held.
  function next(list) {
    return list.find((p) => p.state === "new" && !held(list, p)) || null;
  }

  // shown is what a pending message's line under it says it is doing.
  function shown(list, p) {
    if (p.state === "failed") return "failed";
    if (p.state === "queued") return "queued";
    return held(list, p) ? "held" : "sending";
  }

  // blocks is the pending messages sent in where, as the page draws them
  // after the conversation.
  function blocks(list, where) {
    return list.filter((p) => p.where === where).map((p) => ({
      kind: "user", id: "pending:" + p.key, key: p.key, text: p.text, files: [], urls: p.urls,
      pending: shown(list, p), error: p.state === "failed" ? p.error : "",
    }));
  }

  // rehome moves the ones sent in a draft to the session the draft became
  // (sel, at the address to), so a message stays on screen as the page
  // switches to it, and one still waiting to go is sent there.
  function rehome(list, from, to, sel) {
    for (const p of list) if (p.where === from) Object.assign(p, { where: to, sel });
  }

  const api = { arrived, held, next, blocks, rehome };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadPending = api;
})(typeof window !== "undefined" ? window : this);
