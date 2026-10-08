// Runtime smoke test for the web lens. Run: node daemon/web/smoke_test.js
//
// WHY THIS EXISTS: daemon/web has no build step, bundler or lint, so the only
// gate was `node --check`, which is a parse. A parse cannot see a const declared
// inside a function and referenced from module scope — that shipped a lens whose
// sidebar was permanently empty on every load, because fetchWorkspaces caught its
// own ReferenceError and blanked the list. Measured: `node --check` passes on
// that exact mutation; this file fails on it.
//
// It is a SMOKE test, not a DOM: the stubs are the minimum to get app.js
// evaluated and a few paths driven. If app.js starts touching something new at
// load, add a stub rather than deleting the test — the whole value is that it
// evaluates the real file.
"use strict";
const fs = require("fs");
const path = require("path");
const vm = require("vm");

// Let pending promises settle. A macrotask, so queued microtasks drain first.
const tick = () => new Promise((r) => setImmediate(r));

let failures = 0;
function check(what, cond, detail) {
  if (cond) return;
  failures++;
  console.error(`FAIL: ${what}${detail ? " — " + detail : ""}`);
}

// A permissive element: every property access returns another one, so app.js can
// chain DOM calls at load without the test modelling any of it.
// A size read (clientWidth and the like) coerces to 0.
const el = () => new Proxy(function () {}, {
  get: (_t, k) => (k === Symbol.toPrimitive ? () => 0
    : k === "children" || k === "childNodes" ? [] : k === "length" ? 0 : el()),
  set: () => true,
  apply: () => el(),
});

// The daemon the fake browser talks to. windowsReply lets a case control each
// /v1/windows answer in turn, and gate lets it hold one open so two reads can
// resolve OUT OF ORDER. `book` is the bookkeeping the cases read back.
function fakeFetch({ windowsOk = true, windowsOkFor = null, windowsReply = null, gate = null, whoami = null,
  whoamiGate = null }, book) {
  // A fetch that never settles, unless its abort signal fires — the shape of
  // a half-open connection.
  const hang = (signal) => new Promise((_, reject) => {
    if (signal) signal.onabort = () => reject(new Error("aborted"));
  });
  return async (url, opts) => {
    const u = String(url);
    book.fetched.push({ url: u, method: (opts && opts.method) || "GET", body: opts && opts.body });
    if (u.startsWith("/v1/workspaces")) return { ok: true, status: 200, json: async () => [] };
    if (u.startsWith("/v1/whoami")) {
      if (whoami === "hang") return hang(opts && opts.signal);
      if (whoami === null) return { ok: false, status: 404, json: async () => ({}) };
      if (whoamiGate) await whoamiGate;
      return { ok: true, status: 200, json: async () => whoami };
    }
    if (u.startsWith("/v1/windows?")) {
      const n = book.reads++;
      const good = windowsOkFor ? windowsOkFor(n) : windowsOk;
      if (!good) return { ok: false, status: 503, text: async () => "unreadable" };
      const hold = gate ? gate(n) : null;
      return { ok: true, status: 200, json: async () => {
        if (hold) await hold;
        return windowsReply
          ? windowsReply(n)
          : [{ id: "win-here", name: "Here", workspaceIds: [], openBy: ["p"], open: true, openHere: true }];
      } };
    }
    return { ok: true, status: 200, text: async () => "", json: async () => ({}) };
  };
}

// The fake browser: the minimum globals app.js touches at load. Timers are
// collected into `book`, never fired on their own: a case fires them itself
// (fireTimers) to prove a timeout does what it claims.
function makeContext(opts, book) {
  const { storage = {}, pendingNav = null } = opts;
  return {
    console: { log() {}, warn() {}, error: (...a) => { book.errors.push(a.map(String).join(" ")); }, info() {} },
    document: new Proxy({}, { get: () => el() }),
    localStorage: {
      getItem: (k) => (k in storage ? storage[k] : null),
      setItem: (k, v) => { storage[k] = String(v); },
      removeItem: (k) => { delete storage[k]; },
    },
    crypto: { randomUUID: () => "test-device-uuid" },
    location: { href: "http://x/", origin: "http://x", search: "", pathname: "/", protocol: "http:", host: "x" },
    navigator: { userAgent: "node", serviceWorker: {
      register: () => { book.swRegistrations++; return Promise.resolve({ pushManager: { getSubscription: async () => null } }); },
      addEventListener() {},
    } },
    WebSocket: function (url) { book.sockets.push(String(url)); return el(); },
    PushManager: function () {},
    // xterm.js, for the deep-link attach case: a terminal that accepts anything.
    Terminal: function () { return el(); },
    FitAddon: { FitAddon: function () { return el(); } },
    caches: { open: async () => ({
      match: async (k) => (pendingNav && k === "/__ccmux_pending_nav" ? { text: async () => pendingNav } : undefined),
      delete: async () => true,
    }) },
    setInterval: () => 0, setTimeout: (fn) => { book.timers.push(fn); return book.timers.length; }, clearInterval() {}, clearTimeout() {},
    addEventListener() {}, matchMedia: () => ({ matches: false, addEventListener() {} }),
    prompt: () => { book.prompts++; return "Patric"; }, alert() {}, confirm: () => true,
    AbortController: function () { const s = { onabort: null }; this.signal = s; this.abort = () => { if (s.onabort) s.onabort(); }; },
    Notification: { permission: "default" },
    URLSearchParams, URL, TextEncoder, TextDecoder,
    atob: (x) => x, btoa: (x) => x,
    fetch: fakeFetch(opts, book),
  };
}

// Boots app.js (and push.js when asked) in a fresh fake browser and hands
// back the handles the cases assert on.
function run(opts = {}) {
  const { withPush = false, withBoard = false } = opts;
  const book = { fetched: [], errors: [], sockets: [], timers: [], swRegistrations: 0, reads: 0, prompts: 0 };
  const ctx = makeContext(opts, book);
  ctx.window = ctx; ctx.globalThis = ctx; ctx.self = ctx;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, "app.js"), "utf8"), ctx, { filename: "app.js" });
  if (withPush) vm.runInContext(fs.readFileSync(path.join(__dirname, "push.js"), "utf8"), ctx, { filename: "push.js" });
  // board.js runs after agentchat.js on the page and leans on it; same here.
  for (const f of withBoard ? ["agentchat.js", "board.js"] : []) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, f), "utf8"), ctx, { filename: f });
  }
  // `state` is a top-level const, so it is a lexical binding and never lands on
  // the context object; only function declarations do. Later scripts in the same
  // context DO see it.
  const evalIn = (code) => vm.runInContext(code, ctx);
  const fireTimers = () => { const due = book.timers.splice(0); for (const fn of due) fn(); };
  return { ctx, fetched: book.fetched, errors: book.errors, evalIn, prompts: () => book.prompts, fireTimers,
    sockets: book.sockets, swRegistrations: () => book.swRegistrations };
}

// --- board.js: the attention board's rules. The Mac app's AttentionBoard
// has the same rules and its tests run only on a Mac, so the table lives in
// both places; keep them saying the same thing.
async function boardCases() {
  const b = run({ withBoard: true });
  await tick();
  const R = b.ctx.ccmuxBoard.rules;
  const c = (pane, state, reason, since, name) => ({ pane, state, reason, since, name: name || pane });
  const turn = c("turn", "done", "finished", 100);
  const perm = c("perm", "needs_input", "permission", 300);
  const old = c("old", "needs_input", "", 0);
  const nudge = c("nudge", "needs_input", "finished", 50);
  const order = [turn, perm, old, nudge].sort(R.claimOrder).map((x) => x.pane);
  check("board: blocked first, then the longest wait", order.join() === "old,perm,nudge,turn", order.join());
  check("board: a needs_input with no reason is blocked", R.isBlocked(old) && !R.isBlocked(nudge));
  check("board: one tile on a phone, four on a laptop, six on a big screen",
    R.capFor(390) === 1 && R.capFor(1100) === 4 && R.capFor(1600) === 6);

  const snoozed = new Map();
  const free = R.layout([turn, perm, old], { cap: 2, active: null, prevVisible: [], snoozed, now: 1 });
  check("board: the cap splits tiles from the strip",
    free.visible.map((x) => x.pane).join() === "old,perm" && free.waiting.map((x) => x.pane).join() === "turn");

  const arrival = c("new", "needs_input", "question", 5);
  const frozen = R.layout([turn, perm, arrival], { cap: 2, active: "perm", prevVisible: free.visible, snoozed, now: 1 });
  check("board: an active tile freezes the set; arrivals wait, a claim dealt with elsewhere stays marked",
    frozen.visible.map((x) => x.pane + (x.handled ? "!" : "")).join() === "old!,perm" &&
    frozen.waiting.map((x) => x.pane).join() === "new,turn", JSON.stringify(frozen));

  snoozed.set("perm", { since: 300, until: 1000 });
  const later = R.layout([perm], { cap: 4, active: null, prevVisible: [], snoozed, now: 10 });
  const renewed = R.layout([c("perm", "needs_input", "permission", 400)], { cap: 4, active: null, prevVisible: [], snoozed, now: 10 });
  const expired = R.layout([perm], { cap: 4, active: null, prevVisible: [], snoozed, now: 2000 });
  check("board: not now hides a claim until it changes or 30 minutes pass",
    later.visible.length === 0 && renewed.visible.length === 1 && expired.visible.length === 1);

  // The count the sidebar row shows comes from the firehose's claims, for the
  // window's own live panes only.
  b.evalIn(`state.workspaces = [
    { id: "w1", name: "app", group: "ChartLabs", status: "live", panes: [{ id: "p1" }, { id: "p2" }] },
    { id: "w2", name: "x-post", group: "ChartLabs", status: "live", panes: [{ id: "p3", agent: "x-post", harness: "claude" }] },
    { id: "w3", name: "other", group: "Elsewhere", status: "live", panes: [{ id: "p4" }] },
  ];
  noteAttention("w1", "p1", "needs_input", "permission", 10);
  noteAttention("w1", "p2", "running", "", 0);
  noteAttention("w2", "p3", "done", "finished", 20);
  noteAttention("w3", "p4", "done", "finished", 30);
  noteAttention("w1", "gone", "done", "finished", 5);`);
  check("board: the row counts the window's waiting panes", b.ctx.ccmuxBoard.count("ChartLabs") === 2,
    String(b.ctx.ccmuxBoard.count("ChartLabs")));
  check("board: a firehose claim keeps its reason and start",
    b.evalIn(`state.claims.w1.p1.reason === "permission" && state.claims.w1.p1.since === 10`));

  // Switching tiles: the same steps as the Mac's AttentionBoardModelTests.
  const s = run({ withBoard: true });
  await tick();
  s.evalIn(`state.workspaces = ["a", "b", "c", "d", "e"].map((n) => (
    { id: "w" + n, name: n, group: "W", status: "live", panes: [{ id: n }] }));
  ["a", "b", "c", "d"].forEach((n, i) => noteAttention("w" + n, n, "needs_input", "permission", 10 + i));`);
  // A laptop-wide main area: four tiles, as in the Mac test.
  const wide = new Proxy(function () {}, { get: (_t, k) => (k === "clientWidth" ? 1100 : el()[k]), set: () => true, apply: () => el() });
  const page = s.ctx.document;
  const looks = { getElementById: (id) => (id === "main" ? wide : el()), visibilityState: "visible", hasFocus: () => true };
  s.ctx.document = new Proxy({}, { get: (_t, k) => (k in looks ? looks[k] : page[k]) });
  const B = s.ctx.ccmuxBoard;
  B.open("W");
  B.activate("a");

  // A tile is not a look at its workspace: the attach keeps the claim, and
  // focus names no pane, from a screen that is visible and focused. Off the
  // board the same lens does name the pane it shows.
  check("board: opening a tile keeps its claim", s.evalIn(`(state.attn.wa || {}).a === "needs_input"`));
  const lastFocus = () => s.evalIn(`(() => {
    const sent = [];
    state.conn = { readyState: 1, send: (f) => sent.push(JSON.parse(f)) };
    state.paneId = "a";
    reportFocus();
    return sent.pop();
  })()`);
  const onBoard = lastFocus();
  check("board: a tile reports presence and names no pane", onBoard.pane === "" && onBoard.present === true, JSON.stringify(onBoard));
  s.evalIn(`state.board = ""`);
  const offBoard = lastFocus();
  check("board: off the board the lens names the pane it shows", offBoard.pane === "a", JSON.stringify(offBoard));
  s.evalIn(`state.board = "W"; state.conn = null`);

  s.evalIn(`noteAttention("we", "e", "needs_input", "permission", 1)`);
  B.refresh();
  const held = B.peek();
  check("board: an arrival while a tile is active waits in the strip",
    held.visible.join() === "a,b,c,d" && held.waiting.join() === "e", JSON.stringify(held));
  // Moving on sends "acted" on the active tile's open socket; a recorder
  // stands in for it. With no open socket nothing is sent.
  const sent = [];
  s.ctx.boardSent = sent;
  const liveSocket = (pane) =>
    s.evalIn(`state.conn = { readyState: 1, send: (f) => boardSent.push(JSON.parse(f)), close() {} }; state.paneId = "${pane}"`);
  const claimOf = (pane) => s.evalIn(`state.claims["w${pane}"]["${pane}"].state`);
  liveSocket("a");
  B.activate("d");
  B.refresh();
  const switched = B.peek();
  check("board: switching tiles moves on from the old one without re-sorting",
    switched.active === "d" && switched.visible.join() === "a!,b,c,d" && sent.map((f) => f.t + ":" + f.pane).join() === "acted:a",
    JSON.stringify({ switched, sent }));
  B.activate("a");
  check("board: a handled tile can still be opened", B.peek().active === "a", JSON.stringify(B.peek()));

  B.activate("b");
  liveSocket("b");
  B.next();
  check("board: Next sends acted for the tile left and retires it here",
    sent.map((f) => f.pane).join() === "a,b" && claimOf("b") === "idle", JSON.stringify(sent));
  const leaving = B.peek().active;
  s.evalIn(`state.conn = null`);
  B.next();
  check("board: with no open socket, moving on sends nothing and the claim stays",
    sent.length === 2 && claimOf(leaving) === "needs_input", `${leaving}: ${JSON.stringify(sent)}`);
  const parked = B.peek().active;
  liveSocket(parked);
  B.snooze({ pane: parked, since: s.evalIn(`state.claims["w${parked}"]["${parked}"].since`) });
  check("board: Not now on the active tile is not dealing with it",
    sent.length === 2 && B.peek().active === null && claimOf(parked) === "needs_input", `${parked}: ${JSON.stringify(sent)}`);

  // Sizing: the same table as the Mac's testAScreenTileHoldsItsPaneAtTheBoardsTextSize.
  const cell = { w: 7.2, h: 14.4 };
  const g = R.tileGrid(634, 350, cell);
  check("board: a screen tile holds its pane at the board's text size",
    g && g.cols === 88 && g.rows === 24 && R.tileGrid(140, 350, cell) === null, JSON.stringify(g));
  const f = (cols, rows) => Math.round(R.fontFor(634, 350, cols, rows, cell) * 100) / 100;
  check("board: a pane bigger than its tile shrinks to show whole, never below 4",
    f(88, 24) === 12 && f(176, 24) === 6 && f(88, 48) === 6.08 && R.fontFor(100, 100, 400, 100, cell) === 4,
    [f(88, 24), f(176, 24), f(88, 48)].join());

  // And the Mac's testATileSizesItsPaneOncePerSize. The fake page cannot
  // measure a font, so the cell is the 0.6em fallback: 7.2 x 14.4.
  const z = run({ withBoard: true });
  await tick();
  z.evalIn(`state.workspaces = [
    { id: "wz", name: "z", group: "Z", status: "live", panes: [{ id: "z" }] },
    { id: "wq", name: "q", group: "Z", status: "live", panes: [{ id: "q", agent: "q", harness: "claude" }] },
  ];
  noteAttention("wz", "z", "needs_input", "permission", 1);
  noteAttention("wq", "q", "needs_input", "permission", 2);`);
  const zPage = z.ctx.document;
  z.ctx.document = new Proxy({}, { get: (_t, k) => (k in looks ? looks[k] : zPage[k]) });
  const Z = z.ctx.ccmuxBoard;
  Z.open("Z");
  const resizes = () => z.fetched.filter((x) => x.method === "POST" && x.url.endsWith("/resize"))
    .map((x) => x.url.split("/")[3] + " " + JSON.parse(x.body).cols + "x" + JSON.parse(x.body).rows);
  Z.sizePane("z", 634, 350);
  Z.sizePane("z", 634, 350); // the next paint, same tile
  Z.sizePane("q", 634, 350); // a chat has no screen to size
  Z.sizePane("z", 900, 350); // the tile grew
  await tick();
  check("board: a tile sizes its pane once per size, and a chat tile not at all",
    resizes().join() === "z 88x24,z 125x24", resizes().join());
  const okFetch = z.ctx.fetch;
  const answering = (status) => async (u, o) => {
    z.fetched.push({ url: String(u), method: o.method, body: o.body });
    return { ok: false, status, text: async () => `{"error":"no ${status}"}` };
  };
  z.ctx.fetch = answering(500);
  Z.sizePane("z", 634, 350);
  await tick();
  Z.sizePane("z", 634, 350);
  await tick();
  check("board: a size that failed is sent again on the next paint",
    resizes().join() === "z 88x24,z 125x24,z 88x24,z 88x24", resizes().join());
  check("board: a 4xx is a refusal that stands, a 5xx or no answer is tried again",
    R.sizeOutcome(200) === "sized" && R.sizeOutcome(404) === "refused" && R.sizeOutcome(409) === "refused" &&
    R.sizeOutcome(500) === "failed" && R.sizeOutcome(0) === "failed");
  z.ctx.fetch = answering(404); // an older host without the route
  Z.sizePane("z", 634, 350);
  await tick();
  Z.sizePane("z", 634, 350);
  await tick();
  check("board: a size the daemon refused is not sent again",
    resizes().length === 5, resizes().join());
  z.ctx.fetch = okFetch;

  // A tile that leaves the board forgets its size; so does a closed board.
  // Either way the next showing sizes the pane again, whatever another lens
  // did with it meanwhile. The Mac's testATileThatComesBackIsSizedAgain.
  Z.sizePane("z", 900, 350);
  z.evalIn(`noteAttention("wz", "z", "idle", "", 0)`);
  Z.refresh();
  z.evalIn(`noteAttention("wz", "z", "needs_input", "permission", 5)`);
  Z.refresh();
  Z.sizePane("z", 900, 350);
  await tick();
  check("board: a tile that comes back is sized again", resizes().slice(5).join() === "z 125x24,z 125x24",
    resizes().join());
  Z.close();
  Z.open("Z");
  Z.sizePane("z", 900, 350);
  await tick();
  check("board: a reopened board sizes its panes again", resizes().length === 8, resizes().join());
}

// --- peers.js: the message panel's history merge (PeerMessage.merged and
// liveRow on the Mac, whose tests run only on a Mac). Its own tiny DOM: rows
// need children, dataset and replaceChildren, which the permissive Proxy above
// cannot answer.
class PeersClassList {
  constructor() { this.s = new Set(["hidden"]); }
  add(c) { this.s.add(c); }
  remove(c) { this.s.delete(c); }
  toggle(c, force) { if (force === undefined ? !this.s.has(c) : force) this.s.add(c); else this.s.delete(c); }
  contains(c) { return this.s.has(c); }
}

class PeersEl {
  constructor() {
    Object.assign(this, { dataset: {}, classList: new PeersClassList(), children: [], textContent: "",
      className: "", title: "", scrollTop: 0, clientHeight: 0, scrollHeight: 0, html: "" });
  }
  set innerHTML(v) { this.html = v; if (v === "") this.children = []; }
  get innerHTML() { return this.html; }
  appendChild(c) { this.children.push(c); return c; }
  replaceChildren(...cs) { this.children = cs; }
  querySelector(sel) {
    const m = /data-seq="(\d+)"/.exec(sel);
    return (m && this.children.find((c) => c.dataset.seq === m[1])) || null;
  }
}

// One panel against a fake bus: `replies` answers history reads in turn (a
// `gate` holds one open, `fail` refuses it), and the socket is driven by hand.
function runPeers() {
  const els = {};
  const sockets = [];
  const timers = [];
  const replies = [];
  const warns = [];
  class FakeWS { constructor(url) { this.url = url; sockets.push(this); } close() { this.closed = true; } }
  const history = async () => {
    const r = replies.shift() || { msgs: [] };
    if (r.gate) await r.gate;
    if (r.unreachable) throw new TypeError("Failed to fetch");
    if (r.fail) return { ok: false, status: 503, json: async () => ({}) };
    return { ok: true, status: 200, json: async () => r.msgs };
  };
  const ctx = {
    console: { ...console, warn: (...a) => warns.push(a) },
    location: { protocol: "http:", host: "lens" },
    setTimeout: (f) => { timers.push(f); return f; },
    clearTimeout: (f) => { const i = timers.indexOf(f); if (i >= 0) timers.splice(i, 1); },
    WebSocket: FakeWS,
    document: { getElementById: (id) => els[id] || (els[id] = new PeersEl()), createElement: () => new PeersEl() },
    fetch: async (url) => {
      const u = String(url);
      if (u === "/v1/peers/viewer") return { ok: true, status: 200, json: async () => ({ bus: "", token: "" }) };
      if (u.startsWith("/v1/peers/messages")) return history();
      return { ok: true, status: 200, json: async () => [] };
    },
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, "peers.js"), "utf8"), ctx, { filename: "peers.js" });
  const sock = () => sockets[sockets.length - 1];
  return {
    replies, timers,
    open: (g) => ctx.ccmuxPeers.open(g),
    close: () => els["peers-close"].onclick(),
    hello: () => sock().onmessage({ data: '{"type":"hello"}' }),
    live: (seq) => sock().onmessage({ data: JSON.stringify({ type: "message", seq, from_id: "a", to_id: "b", text: "m" + seq, sent_at: "" }) }),
    drop: () => sock().onclose(),
    rows: () => els["peers-msgs"].children.map((c) => Number(c.dataset.seq)),
    sockets: () => sockets.length,
    warns,
    status: () => els["peers-status"],
    note: () => els["peers-note"],
  };
}

const peersHist = (...ids) => ids.map((id) => ({ id, from_id: "a", to_id: "b", text: "m" + id, sent_at: "" }));
const held = () => { let release; const gate = new Promise((r) => { release = r; }); return { gate, release }; };
const settle = async () => { for (let i = 0; i < 6; i++) await tick(); };

// Every hello reads history back and merges it into the rows on screen by
// message number; these are the races that merge exists for.
async function peersMergeCases() {
  const p = runPeers();
  p.replies.push({ msgs: peersHist(1, 2) });
  p.open("g");
  await settle();
  check("peers: the open reads history", p.rows().join() === "1,2", p.rows().join());
  // The read-back is held while live 4 lands; it then brings 3.
  const h = held();
  p.replies.push({ msgs: peersHist(1, 2, 3), gate: h.gate });
  p.hello();
  p.live(4);
  h.release();
  await settle();
  check("peers: a read-back keeps a live row that landed while it was out", p.rows().join() === "1,2,3,4", p.rows().join());
  p.live(3);
  check("peers: a live copy of a row already read back is not drawn twice", p.rows().join() === "1,2,3,4", p.rows().join());

  // Drop, redial, and a read-back that fails: the list stays, a note says why.
  p.drop();
  p.timers.shift()();
  p.replies.push({ fail: true });
  p.hello();
  await settle();
  check("peers: a failed read-back keeps the list", p.rows().join() === "1,2,3,4", p.rows().join());
  check("peers: a failed read-back says the list may have a hole",
    !p.note().classList.contains("hidden") && p.note().textContent.includes("may be missing"), p.note().textContent);

  // A good read clears the note; one that comes back full leaves no unmarked
  // hole between it and the older rows on screen, yet still keeps a live row
  // that landed while it was out (what tells this rule from a plain replace).
  const full = held();
  p.replies.push({ msgs: peersHist(...Array.from({ length: 200 }, (_, i) => 1001 + i)), gate: full.gate });
  p.hello();
  p.live(1201);
  full.release();
  await settle();
  check("peers: a good read clears the note", p.note().classList.contains("hidden"), p.note().textContent);
  const r = p.rows();
  check("peers: a full read drops the rows older than it and keeps the newer live one",
    r[0] === 1001 && r[r.length - 1] === 1201 && r.length === 201, `first ${r[0]}, last ${r[r.length - 1]}, ${r.length} rows`);
}

async function peersFailureCases() {
  // A read that lands after a close and reopen writes nothing.
  const s = runPeers();
  const stale = held();
  s.replies.push({ msgs: peersHist(50), gate: stale.gate });
  s.open("g");
  await settle();
  s.close();
  s.replies.push({ msgs: peersHist(60) });
  s.open("g");
  await settle();
  stale.release();
  await settle();
  check("peers: a read from before a reopen writes nothing", s.rows().join() === "60", s.rows().join());

  // A redial pending from before a close does not dial for the next open.
  s.drop();
  s.close();
  s.open("g");
  await settle();
  check("peers: a close cancels the pending redial", s.timers.length === 0, `${s.timers.length} timer(s) left`);

  // A first read that fails is the panel's error, as on the Mac, not a gap.
  const f = runPeers();
  f.replies.push({ fail: true });
  f.open("g");
  await settle();
  check("peers: a failed first read shows the refusal",
    !f.status().classList.contains("hidden") && f.status().textContent.includes("can't reach the hub"), f.status().textContent);
  check("peers: a failed first read is not called a gap", f.note().classList.contains("hidden"), f.note().textContent);
  check("peers: a failed first read says how to retry", f.status().textContent.endsWith("Close and reopen to try again."), f.status().textContent);
  // As on the Mac, no stream after a failed open: live rows would hide the
  // error and pass for the whole history.
  check("peers: a failed first read opens no live stream", f.sockets() === 0, `${f.sockets()} socket(s)`);

  // No answer at all is named as such, not as the browser's own words.
  const u = runPeers();
  u.replies.push({ unreachable: true });
  u.open("g");
  await settle();
  check("peers: an unreachable daemon is named", u.status().textContent.startsWith("Cannot reach ccmuxd"), u.status().textContent);
  // The browser's own error is logged, not dropped with the friendlier text.
  check("peers: a failed read logs its cause", u.warns.some((a) => a.some((x) => x && String(x.cause).includes("Failed to fetch"))),
    `${u.warns.length} warning(s)`);
}

(async () => {
  // 1. The lens loads and its window path is reachable from module scope. This
  //    is the ReferenceError check: a mis-scoped const makes fetchWorkspaces
  //    throw into its own catch, and nothing below ever runs.
  const ok = run();
  check("fetchWorkspaces is reachable", typeof ok.ctx.fetchWorkspaces === "function");
  await ok.ctx.fetchWorkspaces();
  await tick();
  const list = ok.fetched.find((f) => f.url.startsWith("/v1/windows?"));
  check("the window list names this device", list && list.url.includes("device=test-device-uuid"),
    list ? list.url : "no /v1/windows call at all");

  // 2. This lens must NEVER declare an open-set. What it renders as open IS the
  //    daemon's own openHere, so a declaration could omit nothing (no repair)
  //    while its re-sends refreshed last_seen forever — disabling the TTL that
  //    is a stranded tab's only cleanup. Read the comment in app.js before
  //    adding one back.
  check("no open-set declaration", !ok.fetched.some((f) => f.url.includes("open-set")),
    "the web lens declared a set it cannot evidence");

  // 3. Open and close name the device, so one lens cannot clear another's row.
  const acted = run();
  await acted.ctx.fetchWorkspaces();
  await tick();
  const win = acted.evalIn("state.windows[0]");
  // Only calls made AFTER this point count. The read above already sent a
  // keep-alive to the same /open route, and a `find` over everything matched
  // that one instead — so deleting device= from openWindow still passed.
  const before = acted.fetched.length;
  await acted.ctx.openWindow(win);
  await tick();
  await acted.ctx.closeWindow(win);
  await tick();
  const acts = acted.fetched.slice(before);
  for (const verb of ["open", "close"]) {
    const call = acts.find((f) => f.method === "POST" && f.url.includes(`/${verb}?`));
    check(`${verb} names the device`, call && call.url.includes("device=test-device-uuid"),
      call ? call.url : `no ${verb} POST`);
  }

  // 4. Out-of-order reads: the OLDER answer must not overwrite the newer one.
  //    fetchWorkspaces has no in-flight guard and runs from a timer, the
  //    firehose and every mutation, so a late answer landing last leaves the
  //    lens showing state the daemon has already moved past — and callers that
  //    await a read and then attach act on the stale list.
  const holds = [];
  const race = run({
    gate: () => new Promise((r) => holds.push(r)),
    // The two reads must DIFFER, or the assertion cannot tell which landed.
    windowsReply: (n) => [{
      id: "win-here", name: n === 0 ? "Stale" : "Fresh", workspaceIds: [],
      openBy: ["p"], open: true, openHere: n !== 0,
    }],
  });
  const first = race.ctx.fetchWorkspaces();
  await tick();
  const second = race.ctx.fetchWorkspaces();
  await tick();
  holds.reverse().forEach((release) => release()); // newer resolves FIRST
  await Promise.all([first, second]);
  await tick();
  check("a stale read does not overwrite a newer one",
    race.evalIn("state.windows[0].name") === "Fresh",
    `state holds ${race.evalIn("JSON.stringify(state.windows[0].name)")}`);

  // 5. The mirror hazard: gating on the newest ISSUED read starves. Reads fire
  //    from a timer and from every firehose frame, so if latency exceeds the gap
  //    between triggers, every response is superseded before it resolves and
  //    NOTHING is ever applied — the lens silently freezes.
  const starve = [];
  const slow = run({
    gate: () => new Promise((r) => starve.push(r)),
    windowsReply: () => [{
      id: "win-here", name: "Applied", workspaceIds: [], openBy: ["p"], open: true, openHere: true,
    }],
  });
  const a = slow.ctx.fetchWorkspaces(); // older read
  await tick();
  slow.ctx.fetchWorkspaces();           // newer read ISSUED but never resolved
  await tick();
  starve[0]();                          // only the OLDER one comes back
  await a;
  await tick();
  check("an older read still applies when nothing newer has landed",
    slow.evalIn("(state.windows[0] || {}).name") === "Applied",
    "a superseded-but-unlanded read was dropped, so state never updates");

  // 6. A failed window read keeps the PREVIOUS list rather than blanking it:
  //    every window would render as closed and feed wrong close decisions.
  //
  //    The first read must succeed, or there is no previous list and the
  //    assertion is vacuous — an earlier version checked Array.isArray on a
  //    state that starts as [], so it passed however the code behaved.
  const bad = run({ windowsOkFor: (n) => n === 0 });
  await bad.ctx.fetchWorkspaces();
  await tick();
  check("the good read landed first", bad.evalIn("(state.windows[0] || {}).name") === "Here");
  await bad.ctx.fetchWorkspaces();
  await tick();
  check("an unreadable window list does not blank the previous one",
    bad.evalIn("(state.windows[0] || {}).name") === "Here",
    `state holds ${bad.evalIn("JSON.stringify(state.windows)")}`);
  //    And it is LOGGED with the daemon's status and body: a persistent 503
  //    otherwise leaves a stale list, a clean console, and no keep-alive.
  check("an unreadable window list is logged with status and body",
    bad.errors.some((e) => e.includes("503") && e.includes("unreadable")),
    `console.error calls: ${JSON.stringify(bad.errors)}`);

  // 7. openHere falls back to open, for a daemon older than the field. Without
  //    it a newer lens reads every window as closed and clicking one opens a
  //    duplicate onto a shared window.
  const fb = run();
  check("openHere absent falls back to open", fb.evalIn("openHereOf({ open: true })") === true,
    "an older daemon's windows would all read as closed here");
  check("openHere present wins over open", fb.evalIn("openHereOf({ open: true, openHere: false })") === false,
    "the per-login flag overrode this lens's own");

  // 8. The keep-alive. Every flag expires after the daemon's TTL and the only
  //    other writers here are the user clicking open or close, so a tab left
  //    open and in use would age out of its own flags — then render its live
  //    windows as closed and let another lens force-archive them. It must go
  //    through the ADDITIVE open route, never a declaration: additive cannot
  //    retract, so it is harmless against a stale read.
  const alive = run();
  await alive.ctx.fetchWorkspaces();
  await tick();
  await tick();
  const keep = alive.fetched.filter((f) => f.method === "POST" && f.url.includes("/open?"));
  check("a read re-asserts this device's open flags", keep.length === 1,
    `${keep.length} keep-alive posts`);
  check("the keep-alive names the device", keep[0] && keep[0].url.includes("device=test-device-uuid"),
    keep[0] ? keep[0].url : "none");
  check("the keep-alive never declares", !alive.fetched.some((f) => f.url.includes("open-set")),
    "a declaration can retract rows; a keep-alive must not be able to");
  // Once only: a 5s poll must not re-post every tick.
  await alive.ctx.fetchWorkspaces();
  await tick();
  await tick();
  const again = alive.fetched.filter((f) => f.method === "POST" && f.url.includes("/open?"));
  check("the keep-alive is rate limited", again.length === 1,
    `${again.length} posts after a second read — every poll would re-assert`);

  // 6. Identity at boot. A vouched /v1/whoami answer IS the presence name and
  //    the prompt never fires; the call carries the stored name so the daemon's
  //    alias tier can see it. A daemon too old for the endpoint (404) must not
  //    stall the boot: hosts and the firehose still come up, and the prompt is
  //    back to being the only name there is.
  const vouched = run({
    whoami: { login: "carol@example.com", display: "Carol", verified: true, vouched: true },
    storage: { "ccmux-user": "Patric Sandelin" },
  });
  await tick();
  await tick();
  check("a vouched identity is the presence name", vouched.ctx.getUser() === "Carol",
    `getUser() = ${vouched.ctx.getUser()}`);
  check("a vouched lens never prompts", vouched.prompts() === 0, `${vouched.prompts()} prompt(s)`);
  const who = vouched.fetched.find((f) => f.url.startsWith("/v1/whoami"));
  check("whoami carries the STORED name for the alias tier", who && who.url.includes("user=Patric%20Sandelin"),
    who ? who.url : "no whoami call");
  check("boot proceeded after identity", vouched.fetched.some((f) => f.url.startsWith("/v1/hosts")),
    "no /v1/hosts call: boot chain did not run");

  // The headline case: a device opening the lens for the FIRST time over the
  // tailnet — nothing stored, and still no prompt. (The seeded case above
  // cannot prove that: its prompt branch is unreachable either way.)
  const fresh = run({ whoami: { login: "carol@example.com", display: "Carol", verified: true, vouched: true, source: "tailscale" } });
  await tick();
  await tick();
  check("a fresh vouched device never prompts", fresh.ctx.getUser() === "Carol" && fresh.prompts() === 0,
    `getUser() = ${fresh.ctx.getUser()}, ${fresh.prompts()} prompt(s)`);
  check("the line names Tailscale", fresh.ctx.whoAmILine().includes("via Tailscale"), fresh.ctx.whoAmILine());

  // An alias-vouched identity is not "this machine's owner": the line names
  // the tier, or it sends a reader to the wrong setting.
  const aliased = run({ whoami: { login: "sandelin@example.com", display: "Patric Sandelin", verified: false, vouched: true, source: "alias" } });
  await tick();
  await tick();
  check("an alias-vouched line names the alias, not the owner",
    aliased.ctx.whoAmILine().includes("alias") && !aliased.ctx.whoAmILine().includes("owner"), aliased.ctx.whoAmILine());

  // A notification tap (push.js's stashed deep-link) attaches at boot, and
  // that attach must carry the vouched name too: it waits for the same
  // identity promise app.js gates its own boot on. The whoami answer is held
  // until the test releases it, so the tap has every chance to run first.
  let release;
  const held = new Promise((r) => { release = r; });
  const tapped = run({
    whoami: { login: "carol@example.com", display: "Carol", verified: true, vouched: true, source: "tailscale" },
    whoamiGate: held, withPush: true, pendingNav: "/?ws=ws-1",
  });
  await tick();
  await tick();
  check("the deep-link attach waits for identity", tapped.sockets.length === 0,
    `${tapped.sockets.length} socket(s) opened before whoami answered: ${tapped.sockets.join(", ")}`);
  release();
  await tick();
  await tick();
  await tick();
  const attachSock = tapped.sockets.find((u) => u.includes("/v1/attach"));
  check("the deep-link attach carries the vouched name", attachSock && attachSock.includes("user=Carol"),
    attachSock || `no attach socket; sockets: ${tapped.sockets.join(", ")}; errors: ${tapped.errors.join(" | ")}`);
  check("a notification tap never prompts", tapped.prompts() === 0, `${tapped.prompts()} prompt(s)`);

  // A 200 that merely echoes the typed name (the daemon's last tier, vouched
  // false) is NOT an identity: the prompt still fires and the echo is not
  // adopted. Dropping the vouched check is what this case fails on.
  const echo = run({ whoami: { login: "typed", display: "typed", verified: false, vouched: false } });
  await tick();
  await tick();
  check("an unvouched echo still prompts", echo.prompts() === 1 && echo.ctx.getUser() === "Patric",
    `getUser() = ${echo.ctx.getUser()}, ${echo.prompts()} prompt(s)`);
  const echoLine = echo.ctx.whoAmILine();
  check("an unvouched answer reads as unidentified, never as a daemon to update",
    echoLine.startsWith("Not identified by Tailscale") && !echoLine.includes("update ccmuxd"), echoLine);

  // Copying a revealed key says how it went. Three outcomes: no clipboard API
  // (a plain-http lens), a refused write, a write that lands. The empty text
  // is the row the code must not touch: nothing to copy, nothing to say.
  const clip = run();
  await tick();
  const outcomes = [];
  const state = { textContent: "" };
  clip.ctx.navigator.clipboard = undefined;
  await clip.ctx.copyToClipboard("", state);
  outcomes.push(state.textContent);
  await clip.ctx.copyToClipboard("sk-ant-x", state);
  outcomes.push(state.textContent);
  clip.ctx.navigator.clipboard = { writeText: async () => { throw new Error("denied"); } };
  await clip.ctx.copyToClipboard("sk-ant-x", state);
  outcomes.push(state.textContent);
  let written = "";
  clip.ctx.navigator.clipboard = { writeText: async (t) => { written = t; } };
  await clip.ctx.copyToClipboard("sk-ant-x", state);
  outcomes.push(state.textContent);
  check("copy: empty text says nothing", outcomes[0] === "", outcomes[0]);
  check("copy: no clipboard API names https", outcomes[1].startsWith("Copy needs https"), outcomes[1]);
  check("copy: a refused write carries its reason", outcomes[2].startsWith("Couldn't copy (denied)"), outcomes[2]);
  check("copy: a landed write confirms and wrote the text", outcomes[3] === "Copied." && written === "sk-ant-x",
    `${outcomes[3]} / wrote ${JSON.stringify(written)}`);

  // A whoami that never answers must not hold the boot: the timeout aborts
  // it, hosts and the firehose come up, and the prompt is the name.
  const hung = run({ whoami: "hang" });
  await tick();
  check("boot waits on identity, not past it", !hung.fetched.some((f) => f.url.startsWith("/v1/hosts")),
    "hosts fetched before whoami settled — the gate is not there");
  hung.fireTimers();
  await tick();
  await tick();
  check("a hung whoami is abandoned by the timeout", hung.fetched.some((f) => f.url.startsWith("/v1/hosts")),
    "no /v1/hosts call after the timeout fired: the lens would hang blank");
  check("a hung whoami falls back to the prompt", hung.ctx.getUser() === "Patric" && hung.prompts() === 1,
    `getUser() = ${hung.ctx.getUser()}, ${hung.prompts()} prompt(s)`);
  // "The daemon did not answer" is not "the daemon does not know you": the
  // line must say the lookup failed, not send a reader to Tailscale settings.
  check("a failed lookup is reported as such", hung.ctx.whoAmILine().startsWith("Couldn't reach ccmuxd")
    && !hung.ctx.whoAmILine().includes("Not identified"), hung.ctx.whoAmILine());

  // push.js waits for identity ONLY at the deep-link attach: the service
  // worker registers and the settings sheet wires while whoami is still out.
  const slowPush = run({ whoami: "hang", withPush: true });
  await tick();
  await tick();
  check("push.js registers the service worker before identity answers", slowPush.swRegistrations() === 1,
    `${slowPush.swRegistrations()} registration(s) while whoami hung`);

  const old = run();
  await tick();
  await tick();
  check("an old daemon still boots", old.fetched.some((f) => f.url.startsWith("/v1/hosts")),
    "no /v1/hosts call after a 404 from whoami");
  check("an old daemon falls back to the prompt", old.ctx.getUser() === "Patric" && old.prompts() === 1,
    `getUser() = ${old.ctx.getUser()}, ${old.prompts()} prompt(s)`);
  const oldLine = old.ctx.whoAmILine();
  check("a too-old daemon is named as such, not as an error or an unknown caller",
    oldLine.includes("update ccmuxd") && !oldLine.startsWith("Couldn't reach") && !oldLine.startsWith("Not identified"),
    oldLine);

  // The has-chat rule is one function in three places (agent.HasChat in Go,
  // AgentPaneRef.hasChat in Swift, hasChat here); the Go side has TestHasChat
  // with this table, and this keeps the web copy from drifting from it.
  const table = { claude: true, opencode: true, pi: false, codex: false, "": false };
  const drift = Object.entries(table).filter(([h, want]) => ok.ctx.hasChat(h) !== want).map(([h]) => h);
  check("hasChat matches the daemon's HasChat table", drift.length === 0, `drift on: ${drift.join(", ")}`);

  await peersMergeCases();
  await peersFailureCases();
  await boardCases();

  console.log(failures === 0 ? "web lens smoke: ok" : `web lens smoke: ${failures} failure(s)`);
  process.exit(failures === 0 ? 0 : 1);
})();
