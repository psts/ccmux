// threadstream.js: one session's conversation as the thread page holds it,
// built from the frames of GET /v1/sessions/{id}/ws: a snapshot, then the
// runner's numbered events, each folded in once and in order.
//
// The numbers are the point. The page remembers which runner process it is
// reading (boot) and the last event it has (seq), and says so when it dials
// again, so a daemon restart costs nothing and a runner restart costs one
// fresh snapshot. An event numbered at or below seq was seen already and is
// skipped; one that jumps ahead means something was missed, and the page
// asks for a snapshot instead of drawing a conversation with a hole in it.
//
// Plain data in, plain data out (threadstream_test.js); threads.js draws.
(function (root) {
  "use strict";

  function fresh() {
    return { boot: "", seq: 0, turns: [], cards: [], info: {}, result: null, todos: [], busy: false, error: "", where: "", note: "", sdk: "", suggestion: "" };
  }

  const card = (kind, req, at) => ({ kind, id: req.id, req, at });

  // fromSnapshot replaces everything with what a snapshot frame holds. at is
  // when its cards are taken to have been asked (the page has no better
  // time for a card that was up before it looked).
  function fromSnapshot(frame, at) {
    const d = frame.data || {};
    const s = fresh();
    s.boot = frame.boot || "";
    s.seq = frame.seq || 0;
    s.turns = (d.messages || []).map((m) => ({ info: { ...m.info }, parts: (m.parts || []).map((p) => ({ ...p })) }));
    s.cards = [...(d.permissions || []).map((r) => card("permission", r, at)), ...(d.questions || []).map((r) => card("question", r, at))];
    s.info = d.info || {};
    s.result = d.result || null;
    s.todos = d.todos || [];
    s.busy = !!d.busy;
    s.sdk = d.sdk || "";
    s.suggestion = d.suggestion || "";
    return s;
  }

  // offer is the predicted next prompt the empty box shows: only between
  // turns, with nothing typed and no card waiting on an answer.
  function offer(s, typed) {
    if (!s.suggestion || s.busy || s.cards.length || String(typed || "").trim()) return "";
    return s.suggestion;
  }

  const turnOf = (s, id) => s.turns.find((t) => t.info.id === id);

  function ensureTurn(s, id, role) {
    let turn = turnOf(s, id);
    if (!turn) {
      turn = { info: { id, role }, parts: [] };
      s.turns.push(turn);
    }
    return turn;
  }

  const dropCard = (s, id) => { s.cards = s.cards.filter((c) => c.id !== id); return { cards: true }; };

  // One handler per event type: each folds the event's properties into the
  // state and says what must be drawn again.
  const handlers = {
    "message.updated": (s, p) => {
      const turn = ensureTurn(s, p.info.id, p.info.role);
      turn.info = { ...p.info };
      if (p.info.role !== "note") s.busy = true;
      // A failure is the failure of the turn it ended (that turn also shows
      // it, where it happened). The next prompt is a new start, and the page's
      // error bar must not go on reporting what is over.
      if (p.info.role === "user" && !p.info.synthetic) Object.assign(s, { error: "", suggestion: "" }); // a prompt also spends the prediction
      return { turn: turn.info.id };
    },
    "message.part.updated": (s, p) => {
      const turn = ensureTurn(s, p.part.messageID, "assistant");
      const i = turn.parts.findIndex((x) => x.id === p.part.id);
      if (i < 0) turn.parts.push({ ...p.part });
      else turn.parts[i] = { ...p.part };
      return { turn: turn.info.id };
    },
    "message.part.delta": (s, p) => {
      const turn = turnOf(s, p.messageID);
      const part = turn && turn.parts.find((x) => x.id === p.partID);
      if (!part) return {};
      part[p.field] = (part[p.field] || "") + p.delta;
      return { turn: turn.info.id };
    },
    "permission.asked": (s, p, at) => { s.cards.push(card("permission", p, at)); return { cards: true }; },
    "question.asked": (s, p, at) => { s.cards.push(card("question", p, at)); return { cards: true }; },
    "permission.replied": (s, p) => dropCard(s, p.requestID),
    "question.replied": (s, p) => dropCard(s, p.requestID),
    "question.rejected": (s, p) => dropCard(s, p.requestID),
    "session.idle": (s, p) => { s.busy = false; s.result = p.result || s.result; return { meta: true }; },
    "session.error": (s, p) => { s.error = typeof p.error === "string" ? p.error : JSON.stringify(p.error); return { meta: true }; },
    "session.info": (s, p) => { s.info = p.info || {}; return { meta: true }; },
    "session.todos": (s, p) => { s.todos = p.todos || []; return { todos: true }; },
    "session.suggestion": (s, p) => { s.suggestion = p.suggestion || ""; return { meta: true }; },
    "session.context": (s, p) => { s.result = { ...(s.result || {}), context: p.context }; return { meta: true }; },
  };

  // apply folds one frame into s and returns what changed:
  //   {all}     a snapshot replaced everything (the returned state is new)
  //   {turn}    one turn's parts or header
  //   {cards}, {meta}, {todos}
  //   {skip}    an event seen already
  //   {resync}  an event out of order: nothing was applied, dial again for a snapshot
  // The state to go on with is in .state.
  function apply(s, frame, at) {
    if (frame.t === "snapshot") return { all: true, state: Object.assign(fromSnapshot(frame, at), { where: s.where, note: s.note }) };
    if (frame.t === "state") {
      s.where = frame.state || "";
      s.note = frame.note || "";
      return { meta: true, state: s };
    }
    if (frame.t !== "event") return { state: s };
    if (frame.seq <= s.seq) return { skip: true, state: s };
    if (frame.seq !== s.seq + 1) return { resync: true, state: s };
    s.seq = frame.seq;
    const ev = frame.data || {};
    const handle = Object.prototype.hasOwnProperty.call(handlers, ev.type) ? handlers[ev.type] : null;
    return { ...(handle ? handle(s, ev.properties || {}, at) : {}), state: s };
  }

  // cursor is what the page says when it dials: the process it reads and the
  // last event it has. After a resync it says nothing, which gets a snapshot.
  function cursor(s) {
    return { boot: s.boot, after: s.seq };
  }

  const api = { fresh, fromSnapshot, apply, cursor, offer };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadStream = api;
})(typeof window !== "undefined" ? window : this);
