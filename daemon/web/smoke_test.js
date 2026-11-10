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
// wakeRoute answers the window open and the session wakes the way a case asks
// (windowWakeCases), or undefined to leave the request to fakeFetch.
const isOpen = (u) => /^\/v1\/windows\/[^/]+\/open\?/.test(u);
function wakeRoute(u, { openReply = null, reviveFails = false, reviveThrows = [], openDenied = false, agentWake = null, onRevive = null }) {
  // An agent's wake (POST /v1/windows/{id}/agents/{name}): agentWake() wakes it
  // on the fake daemon and returns the pane the route answers with.
  if (agentWake && /^\/v1\/windows\/[^/]+\/agents\//.test(u)) {
    const pane = agentWake();
    return { ok: true, status: 200, json: async () => pane };
  }
  // The daemon's error body, as writeError sends it.
  const refusal = JSON.stringify({ error: "host unreachable" });
  if (reviveFails && u.includes("/revive")) return { ok: false, status: 502, text: async () => refusal, json: async () => JSON.parse(refusal) };
  if ((reviveThrows || []).some((id) => u === `/v1/workspaces/${id}/revive`)) throw new TypeError("Failed to fetch");
  // onRevive wakes the session on the fake daemon, as the real route does
  // before it answers.
  if (onRevive && u.includes("/revive")) { onRevive(u); return { ok: true, status: 200, text: async () => "", json: async () => ({}) }; }
  if (openDenied && isOpen(u)) return { ok: false, status: 403, text: async () => "not a member" };
  if (!openReply || !isOpen(u)) return undefined;
  const json = async () => { if (openReply === "garbled") throw new SyntaxError("not JSON"); return openReply; };
  return { ok: true, status: 200, json };
}

function fakeFetch(opts0, book) {
  const { windowsOk = true, windowsOkFor = null, windowsReply = null, gate = null, whoami = null,
    whoamiGate = null, workspacesReply = null, lastSession = "" } = opts0;
  // A fetch that never settles, unless its abort signal fires — the shape of
  // a half-open connection.
  const hang = (signal) => new Promise((_, reject) => {
    if (signal) signal.onabort = () => reject(new Error("aborted"));
  });
  return async (url, opts) => {
    const u = String(url);
    book.fetched.push({ url: u, method: (opts && opts.method) || "GET", body: opts && opts.body });
    const woke = wakeRoute(u, opts0);
    if (woke) return woke;
    if (u.startsWith("/v1/me/last-session")) return { ok: true, status: 200, json: async () => ({ workspaceId: lastSession }) };
    if (u.startsWith("/v1/workspaces")) return { ok: true, status: 200, json: async () => (workspacesReply ? workspacesReply() : []) };
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
let pageSeq = 0;
function makeContext(opts, book) {
  const { storage = {}, pendingNav = null, search = "", openRefused = false, name = "" } = opts;
  let uuids = 0; // the first is the device id app.js asks for, the next this window's page id
  let stash = pendingNav;
  const location = { href: "http://x/" + search, origin: "http://x", search, pathname: "/", protocol: "http:", host: "x" };
  return {
    console: { log() {}, warn() {}, error: (...a) => { book.errors.push(a.map(String).join(" ")); }, info() {} },
    document: new Proxy({}, { get: () => el() }),
    localStorage: {
      getItem: (k) => (k in storage ? storage[k] : null),
      setItem: (k, v) => { storage[k] = String(v); },
      removeItem: (k) => { delete storage[k]; },
    },
    crypto: { randomUUID: () => (uuids++ === 0 && !storage["ccmux-device"] ? "test-device-uuid" : `page-${++pageSeq}`) },
    location,
    // pagewin.js: this browser window's name (its page id), its address bar,
    // and the windows it asks the browser for (null when the browser refuses one).
    name,
    BroadcastChannel: opts.BroadcastChannel,
    // replaceState moves location too, as in a browser: a page that rewrites
    // its address before reading it must see the rewrite.
    history: { replaceState: (_s, _t, url) => {
      const u = String(url);
      book.addresses.push(u);
      location.search = u.includes("?") ? u.slice(u.indexOf("?")) : "";
    } },
    open: (url) => { book.opened.push(String(url)); return openRefused ? null : {}; },
    // handOff: who opened this browser window, and whether it was raised.
    opener: opts.opener || null, closed: false,
    focus: () => { book.focused++; },
    Response: function (body) { this.body = body; },
    navigator: { userAgent: "node", serviceWorker: {
      register: () => { book.swRegistrations++; return Promise.resolve({ pushManager: { getSubscription: async () => null } }); },
      addEventListener: (type, fn) => { if (type === "message") book.swListeners.push(fn); },
    } },
    // A socket keeps the handlers the page assigns (onclose and the like), so
    // a case can close it; anything else reads as a permissive element.
    WebSocket: function (url) {
      book.sockets.push(String(url));
      const kept = {};
      book.live.push(kept);
      return new Proxy(kept, { get: (t, k) => (k in t ? t[k] : el()), set: (t, k, v) => { t[k] = v; return true; } });
    },
    PushManager: function () {},
    // xterm.js, for the deep-link attach case: a terminal that accepts anything.
    Terminal: function () { return el(); },
    FitAddon: { FitAddon: function () { return el(); } },
    caches: { open: async () => ({
      match: async (k) => (stash && k === "/__ccmux_pending_nav" ? { text: async () => stash } : undefined),
      delete: async (k) => { book.cacheDeletes.push(k); if (k === "/__ccmux_pending_nav") stash = null; return true; },
      put: async (k, v) => { book.cachePuts.push([k, v && v.body]); return true; },
    }) },
    setInterval: () => 0, setTimeout: (fn, ms) => { book.timers.push(fn); book.delays.push(ms); return book.timers.length; }, clearInterval() {}, clearTimeout() {},
    addEventListener() {}, matchMedia: () => ({ matches: !!opts.phone, addEventListener() {} }),
    prompt: () => { book.prompts++; return "Patric"; }, alert: (m) => { book.alerts.push(String(m)); }, confirm: () => true,
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
  const book = { fetched: [], errors: [], sockets: [], live: [], timers: [], delays: [], swRegistrations: 0, reads: 0, prompts: 0,
    addresses: [], opened: [], swListeners: [], cacheDeletes: [], cachePuts: [], focused: 0, alerts: [] };
  const ctx = makeContext(opts, book);
  ctx.window = ctx; ctx.globalThis = ctx; ctx.self = ctx;
  vm.createContext(ctx);
  // Same order as index.html: retry.js, which app.js reads at load, app.js,
  // then the per-window glue that boots it, and windowwake.js, which a
  // window's open and close call (app.js).
  for (const f of ["retry.js", "app.js", "route.js", "pagewin.js", "windowwake.js"]) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, f), "utf8"), ctx, { filename: f });
  }
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
    sockets: book.sockets, live: book.live, delays: book.delays, swRegistrations: () => book.swRegistrations, addresses: book.addresses, opened: book.opened,
    alerts: book.alerts,
    swListeners: book.swListeners, cacheDeletes: book.cacheDeletes, cachePuts: book.cachePuts,
    focused: () => book.focused };
}

// --- scratchpad.js: a session's shared notes. A fake text box and daemon
// stand in for the DOM and /v1/workspaces/{id}/scratchpad. ---
// scratchEnv boots app.js and scratchpad.js over a fake text box and a fake
// daemon (shared between envs, as one daemon is) with the given browser
// storage, so a second env over the same storage is a reload.
async function scratchEnv(daemon, storage) {
  const r = run({ storage });
  await settle();
  const winOn = {};
  r.ctx.addEventListener = (type, fn) => { winOn[type] = fn; };
  vm.runInContext(fs.readFileSync(path.join(__dirname, "scratchpad.js"), "utf8"), r.ctx, { filename: "scratchpad.js" });
  const on = {};
  const ta = { value: "", readOnly: false, selectionStart: 0, selectionEnd: 0, focus() {}, setSelectionRange() {},
    addEventListener: (type, fn) => { on[type] = fn; } };
  const line = { textContent: "", classList: { toggle() {} } };
  const box = { dataset: {}, innerHTML: "", querySelector: (q) => (q === "textarea" ? ta : line) };
  const appEl = el();
  // app.js's $ is a const over document.getElementById, so the fake goes there.
  const byId = (id) => (id === "scratchpad" ? box : id === "app" ? appEl : el());
  r.ctx.document = new Proxy({}, { get: (_t, k) => (k === "getElementById" ? byId : el()) });
  r.ctx.fetch = async (u, o = {}) => {
    const id = /workspaces\/([^/]+)\/scratchpad/.exec(String(u))[1];
    daemon.calls.push({ method: o.method || "GET", id, body: o.body, keepalive: o.keepalive });
    if (daemon.hold && !o.method) await daemon.hold;
    if (daemon.down) return { ok: false, status: 503, text: async () => "unreachable" };
    if (o.method === "PUT") daemon.pads[id] = JSON.parse(o.body).content;
    return { ok: true, status: 200, json: async () => ({ content: daemon.pads[id] || "", updatedAt: 1, updatedBy: "p" }) };
  };
  const puts = () => daemon.calls.filter((c) => c.method === "PUT");
  return { r, ta, on, winOn, line, sp: r.ctx.ccmuxScratch, puts };
}

async function scratchpadCases() {
  const daemon = { pads: { "ws-1": "first notes" }, down: false, hold: null, calls: [] };
  const { r, ta, on, winOn, sp, puts } = await scratchEnv(daemon, {});
  const pads = daemon.pads, calls = daemon.calls;
  r.evalIn(`state.wsId = "ws-1"`);

  sp.show();
  await settle();
  check("showing the notes reads them", ta.value === "first notes" && !ta.readOnly && sp.isOpen(), ta.value);

  ta.value = "first notes, more";
  on.input();
  r.fireTimers();
  await settle();
  check("typing saves after a pause", puts().length === 1 && pads["ws-1"] === "first notes, more", JSON.stringify(puts()));
  r.fireTimers();
  await settle();
  check("nothing new typed, nothing sent", puts().length === 1);

  // Someone else saved: a clean box takes their text, a box with unsaved
  // typing keeps its own.
  pads["ws-1"] = "dasha's version";
  await sp.remote("ws-1");
  check("a save elsewhere reaches a clean box", ta.value === "dasha's version", ta.value);
  ta.value = "mine, unsaved";
  pads["ws-1"] = "dasha again";
  const gets = calls.length;
  await sp.remote("ws-1");
  check("a save elsewhere never overwrites unsaved typing", ta.value === "mine, unsaved" && calls.length === gets, ta.value);
  await sp.remote("ws-2");
  check("another session's save is not this one's", calls.length === gets);

  // Re-reading after a save (this page's own echo, or someone else's) must
  // keep the box writable, and typing done during the read wins.
  ta.value = pads["ws-1"] = "settled";
  on.input();
  r.fireTimers();
  await settle();
  let release;
  daemon.hold = new Promise((res) => { release = res; });
  pads["ws-1"] = "theirs, mid-typing";
  const reading = sp.remote("ws-1");
  await tick();
  check("the box stays writable while it re-reads", ta.readOnly === false);
  ta.value = "typed while it read";
  release();
  daemon.hold = null;
  await reading;
  check("typing during a re-read is kept", ta.value === "typed while it read", ta.value);

  // Leaving saves what is unsaved; only the page going away asks the
  // request to outlive it, and never with a body a browser would refuse.
  ta.value = "mine, unsaved";
  sp.hide();
  await settle();
  const last = puts()[puts().length - 1];
  check("leaving the notes saves them", !sp.isOpen() && pads["ws-1"] === "mine, unsaved" && !last.keepalive,
    JSON.stringify(last));
  sp.show();
  await settle();
  ta.value = "small, page closing";
  winOn.pagehide();
  await settle();
  check("a closing page sends small notes with keepalive", puts()[puts().length - 1].keepalive === true);
  ta.value = "x".repeat(70000);
  winOn.pagehide();
  await settle();
  const big = puts()[puts().length - 1];
  check("notes over 64 KB are sent without keepalive", big.keepalive === false && pads["ws-1"].length === 70000,
    String(big.keepalive));
  sp.hide();
  await settle();

  // A failed read must never let a blank box be saved over the real notes.
  daemon.down = true;
  r.evalIn(`state.wsId = "ws-3"`);
  pads["ws-3"] = "precious";
  sp.show();
  await settle();
  check("a failed read leaves the box read-only and empty", ta.readOnly && ta.value === "", `${ta.readOnly} ${ta.value}`);
  daemon.down = false;
  const before = puts().length;
  on.input();
  r.fireTimers();
  sp.hide();
  await settle();
  check("a failed read saves nothing", puts().length === before && pads["ws-3"] === "precious", pads["ws-3"]);
}

// --- onFirehose: each frame kind goes to its handler; unknown kinds, and
// names an object inherits, do nothing. ---
// Drafts: text a failed save could not deliver is kept in this browser and
// comes back, and saves, when those notes open again; nothing else moves.
async function scratchDraftCases() {
  const daemon = { pads: { "ws-1": "one", "ws-2": "two", "ws-3": "three" }, down: false, hold: null, calls: [] };
  const storage = {};
  const a = await scratchEnv(daemon, storage);
  a.r.evalIn(`state.wsId = "ws-1"`);
  a.sp.show();
  await settle();
  daemon.down = true;
  a.ta.value = "one, typed while away";
  a.on.input();
  a.r.fireTimers();
  await settle();
  a.sp.hide(); // the second try fails too
  await settle();
  daemon.down = false;
  a.r.evalIn(`state.wsId = "ws-2"`);
  a.sp.show();
  await settle();
  check("another session's notes open untouched", a.ta.value === "two" && daemon.pads["ws-2"] === "two", a.ta.value);
  a.sp.hide();
  await settle();
  check("leaving them saves nothing of theirs", !a.puts().some((p) => p.id === "ws-2"));

  // A reload of the page: same browser storage, fresh page.
  const b = await scratchEnv(daemon, storage);
  b.r.evalIn(`state.wsId = "ws-1"`);
  b.sp.show();
  await settle();
  check("the unsaved text comes back", b.ta.value === "one, typed while away", b.ta.value);
  b.r.fireTimers();
  await settle();
  check("and is saved", daemon.pads["ws-1"] === "one, typed while away", daemon.pads["ws-1"]);
  check("and the draft is gone once saved", !Object.keys(storage).some((k) => k.includes("ws-1")), JSON.stringify(storage));

  // A draft the daemon already holds is just dropped: no save, no change.
  storage["ccmux-scratch-draft:ws-3"] = JSON.stringify({ text: "three", base: "old three", at: 1 });
  const before = b.puts().length;
  b.r.evalIn(`state.wsId = "ws-3"`);
  b.sp.show();
  await settle();
  b.r.fireTimers();
  await settle();
  check("a draft equal to the saved notes sends nothing", b.puts().length === before && b.ta.value === "three");
  check("and is cleared", !("ccmux-scratch-draft:ws-3" in storage), JSON.stringify(storage));

  // Someone saved after the draft was typed: their notes stay first, the
  // draft is added below them, and it is not added again on the next open.
  const c = await scratchEnv(daemon, storage);
  c.r.evalIn(`state.wsId = "ws-1"`);
  c.sp.show();
  await settle();
  daemon.down = true;
  c.ta.value = "one, typed while away again";
  c.on.input();
  c.r.fireTimers();
  await settle();
  c.sp.hide();
  await settle();
  daemon.down = false;
  daemon.pads["ws-1"] = "newer from dasha";
  const d = await scratchEnv(daemon, storage);
  d.r.evalIn(`state.wsId = "ws-1"`);
  d.sp.show();
  await settle();
  d.r.fireTimers();
  await settle();
  const kept = daemon.pads["ws-1"];
  check("newer notes from elsewhere are kept first", kept.startsWith("newer from dasha\n\n---- not saved from this browser"), kept);
  check("and the draft is added below them", kept.endsWith("\none, typed while away again"), kept);
  d.sp.hide();
  await settle();
  const e = await scratchEnv(daemon, storage);
  e.r.evalIn(`state.wsId = "ws-1"`);
  e.sp.show();
  await settle();
  e.r.fireTimers();
  await settle();
  check("the draft is added once, not on every open", daemon.pads["ws-1"] === kept, daemon.pads["ws-1"]);
}

// The Scratchpad tab: shown for a repo session, never for an agent's or
// with no session open.
async function scratchTabCases() {
  const r = run();
  await settle();
  const hidden = { v: null };
  const tab = { classList: { toggle: (c, on) => { if (c === "hidden") hidden.v = on; } } };
  r.ctx.document = new Proxy({}, { get: (_t, k) => (k === "getElementById" ? (id) => (id === "scratch-tab" ? tab : el()) : el()) });
  r.ctx.ccmuxScratch = { isOpen: () => false };
  r.evalIn(`state.workspaces = [{ id: "repo" }, { id: "bot", agent: "hq" }]`);
  const shown = (wsId) => { r.evalIn(`state.wsId = ${JSON.stringify(wsId)}; renderScratchTab()`); return hidden.v === false; };
  check("a repo session has the Scratchpad tab", shown("repo"));
  check("an agent's session does not", !shown("bot"));
  check("no session, no tab", !shown(null));
  check("a session not listed yet keeps it", shown("not-listed-yet"));
}

async function firehoseCases() {
  const r = run();
  await settle();
  const feed = (m) => r.ctx.onFirehose({ data: JSON.stringify(m) });
  feed({ t: "hello", attention: [{ workspace: "w1", pane: "p1", state: "needs_input" }] });
  check("hello seeds attention", r.evalIn(`state.attn.w1.p1`) === "needs_input");
  feed({ t: "attention", workspace: "w1", pane: "p1", state: "done" });
  check("attention updates a pane", r.evalIn(`state.attn.w1.p1`) === "done");
  const reads = () => r.fetched.filter((f) => f.url.startsWith("/v1/workspaces")).length;
  const before = reads();
  feed({ t: "workspace-status", workspace: "w1" });
  await settle();
  check("a workspace change re-reads the list", reads() > before);
  let heard = null;
  r.ctx.ccmuxScratch = { remote: (id) => { heard = id; } };
  feed({ t: "workspace-scratchpad", workspace: "w1" });
  check("a scratchpad save reaches the notes", heard === "w1", String(heard));
  const errs = r.errors.length;
  for (const t of ["no-such-kind", "constructor", "toString", "__proto__"]) feed({ t });
  feed("not an object");
  check("unknown kinds do nothing", r.errors.length === errs && r.evalIn(`state.attn.w1.p1`) === "done");
}

// The task queue (tasks.js over tasklist.js): each window's row carries that
// window's counts from GET /v1/tasks, a "tasks-changed" frame reads them
// again, and opening a window's queue leaves the session and comes back out
// on openWorkspace.
async function tasksCases() {
  const r = run({ withBoard: true }); // the page has the board and the chat it leans on
  await settle();
  let reply = { tasks: [], counts: { "win-a": { waiting: 2, done: 3, needs_you: 1 }, "win-b": { waiting: 7 } } };
  const reads = [], puts = [];
  r.ctx.fetch = async (u, opts) => {
    reads.push(String(u));
    if (opts && opts.method === "PUT") puts.push([String(u), opts.body]);
    return { ok: true, status: 200, json: async () => reply, text: async () => "" };
  };
  const made = [];
  // The queue's own box keeps what is drawn into it and the handlers hung on it.
  // A redraw replaces the status line, as innerHTML does in a browser.
  const on = {}, line = { textContent: "", classList: { toggle() {} } };
  let html = "";
  const box = { dataset: {}, classList: { add() {}, remove() {}, toggle() {} },
    get innerHTML() { return html; }, set innerHTML(v) { html = v; line.textContent = ""; },
    contains: () => false, querySelector: (sel) => (sel === ".tq-status" ? line : null), addEventListener: (t, f) => { on[t] = f; } };
  r.ctx.document = new Proxy({}, { get: (_t, k) => (k === "createElement"
    ? () => { const li = { className: "", innerHTML: "", onclick: null, querySelector: () => el() }; made.push(li); return li; }
    : k === "getElementById" ? (id) => (id === "tasks" ? box : el()) : el()) });
  for (const f of ["tasklist.js", "tasks.js"]) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, f), "utf8"), r.ctx, { filename: f });
  }
  await settle();
  check("tasks.js reads the queue at load", reads.some((u) => u.startsWith("/v1/tasks?user=")), reads.join(","));
  const winA = { id: "win-a", name: "Mixed" };
  const row = r.ctx.ccmuxTasks.row(winA);
  check("the row shows its window's waiting and done", row.innerHTML.includes("2 waiting · 3 done"), row.innerHTML);
  check("the row counts what needs a person", row.innerHTML.includes(">1</span>") && row.className.includes("waiting"), row.className);
  const rowB = r.ctx.ccmuxTasks.row({ id: "win-b", name: "ChartLabs" });
  check("another window's row has its own counts", rowB.innerHTML.includes("7 waiting") && !rowB.innerHTML.includes("done"), rowB.innerHTML);
  check("a window with no tasks has a quiet row", !r.ctx.ccmuxTasks.row({ id: "win-c", name: "Empty" }).className.includes("waiting"));
  const before = reads.length;
  r.ctx.onFirehose({ data: JSON.stringify({ t: "tasks-changed" }) });
  await settle();
  check("a tasks-changed frame reads the queue again", reads.length > before);
  reply = { tasks: [{ id: 1, repo: "admin", title: "x", plan: "p", status: "waiting", moves: ["planning"], editable: true }], counts: {} };
  r.evalIn(`state.wsId = "ws-9"`);
  r.evalIn(`state.windows = [{ id: "win-a", name: "Mixed", taskSlots: 1 }]`);
  r.ctx.ccmuxTasks.open(winA);
  await settle();
  check("the header offers how many run at once, at the window's number",
    /data-act="slots"[^]*<option value="1" selected>1 at a time/.test(box.innerHTML), box.innerHTML.slice(0, 600));
  on.change({ target: { dataset: { act: "slots" }, value: "0" } });
  await settle();
  check("picking Paused stores 0 for that window",
    puts.some(([u, b]) => u === "/v1/windows/win-a/slots" && JSON.parse(b).slots === 0), JSON.stringify(puts));
  check("the picker then shows Paused", /<option value="0" selected>Paused/.test(box.innerHTML), box.innerHTML.slice(0, 600));
  const okFetch = r.ctx.fetch;
  r.ctx.fetch = async (u, opts) => { if (opts && opts.method === "PUT") throw new Error("network down"); return okFetch(u, opts); };
  on.change({ target: { dataset: { act: "slots" }, value: "2" } });
  await settle();
  check("a failed change keeps saying why", line.textContent.includes("Couldn't change how many run at once"), line.textContent);
  check("and the picker stays at the stored number", /<option value="0" selected>Paused/.test(box.innerHTML), box.innerHTML.slice(0, 600));
  r.ctx.fetch = okFetch;
  check("opening the task view leaves the session", r.ctx.ccmuxTasks.isOpen() && r.evalIn("state.wsId") === null);
  check("the view holds the window it was opened for", r.ctx.ccmuxTasks.openWindow() === "win-a", r.ctx.ccmuxTasks.openWindow());
  r.evalIn(`openWorkspace("ws-1")`);
  check("opening a session closes the task view", !r.ctx.ccmuxTasks.isOpen());
}

// An open window with no session still gets its section, so its Tasks row
// shows; a closed one does not, and AVAILABLE stays last.
async function emptyWindowCases() {
  const r = run();
  await settle();
  r.evalIn(`state.windows = [
    { id: "win-a", name: "Mixed", open: true, openHere: true },
    { id: "win-e", name: "Emptied", open: true, openHere: true },
    { id: "win-c", name: "Shut", open: false, openHere: false }]`);
  const groups = r.ctx.withEmptyWindows([["Mixed", [{ id: "ws-1" }]], ["", [{ id: "ws-9" }]]]).map(([g, l]) => `${g}:${l.length}`);
  check("an open window with no sessions keeps a section, before AVAILABLE", groups.join(",") === "Mixed:1,Emptied:0,:1", groups.join(","));
}

// --- the tab ×: which tabs get one, and what a click does when the close
// works, fails at the daemon, never reaches it, or is declined. ---
async function closeTabCases() {
  const r = run();
  await settle();
  const alerts = [];
  r.ctx.alert = (m) => alerts.push(String(m));
  r.evalIn(`state.wsId = "ws-1"; state.panes = [{ id: "p1" }, { id: "p2", agent: "hq" }, { id: "p3", harness: "claude" }]`);
  const closable = ["p1", "p2", "p3"].filter((id) => r.evalIn(`closableTab(state.panes.find((p) => p.id === "${id}"))`));
  check("an agent's tab has no ×, the others do", closable.join() === "p1,p3", closable.join());
  r.evalIn(`state.panes = [{ id: "p1" }]`);
  check("the only tab has no ×", r.evalIn(`closableTab(state.panes[0])`) === false);

  const deletes = () => r.fetched.filter((f) => f.method === "DELETE").length;
  const before = deletes();
  r.ctx.confirm = () => false;
  await r.ctx.closePaneTab({ id: "p3", harness: "claude" });
  check("a declined harness close sends nothing", deletes() === before && alerts.length === 0);

  r.ctx.fetch = () => Promise.reject(new TypeError("Failed to fetch"));
  await r.ctx.closePaneTab({ id: "p1" });
  check("a close that never reaches the daemon says so", alerts.length === 1 && alerts[0] === "close tab: Failed to fetch",
    alerts.join(" | "));

  r.ctx.fetch = async () => ({ ok: false, status: 400, text: async () => "unknown pane" });
  await r.ctx.closePaneTab({ id: "p1" });
  check("a close the daemon refuses says why", alerts[1] === "close tab: unknown pane", alerts.join(" | "));

  r.ctx.fetch = async () => ({ ok: true, status: 204 });
  await r.ctx.closePaneTab({ id: "p1" });
  check("a close that works says nothing", alerts.length === 2, alerts.join(" | "));
}

// --- pagewin.js: one ccmux window per browser window. The rules themselves
// are in route_test.js; these drive the real boot through app.js.
const twoWindows = () => [
  { id: "win-a", name: "Mixed", workspaceIds: ["ws-1"], openBy: ["p"], open: true, openHere: true },
  { id: "win-b", name: "ChartLabs", workspaceIds: ["ws-2"], openBy: ["p"], open: true, openHere: true },
];
// Sockets with no workspace id (an attach of a session the page never had) count as none.
const attachedTo = (r) => r.sockets.filter((u) => u.includes("/v1/attach"))
  .map((u) => (/workspace=([^&]+)/.exec(u) || [])[1]).filter(Boolean);
const lastAddress = (r) => r.addresses[r.addresses.length - 1] || "";

// attachOrigin: a session on the daemon serving this page connects through
// the page's own address. Dialing the host's tailnet name instead cost a fresh
// name lookup on every session switch, measured at 1.5s on a Mac after a few
// idle minutes. A session on another host still goes straight to that host.
function attachOriginCases() {
  const r = run();
  r.evalIn(`state.hosts = {
    me: { id: "me", addr: "me.tail.ts.net", self: true },
    other: { id: "other", addr: "other.tail.ts.net" },
  }`);
  const origin = (host) => r.evalIn(`attachOrigin({ host: ${JSON.stringify(host)} })`);
  check("a session on this daemon connects through the page's own address",
    origin("me") === "ws://x", origin("me"));
  check("a session on another host connects straight to it",
    origin("other") === "ws://other.tail.ts.net", origin("other"));
  check("a session with no host stays on the page's address", origin("") === "ws://x", origin(""));
}

// A sleeping session has nothing to attach to: the daemon refuses with 409.
// A window that kept one on screen retried every 2s for as long as it was
// open, and Chrome then held back every new connection from the windows that
// share its process (measured: session switches took 1.5 to 10s). Opening a
// sleeping session wakes it first. One that falls asleep while shown is not
// dialed and not woken behind anyone's back: it waits, and comes back when the
// daemon says it is awake (a short blip heals itself within 30s).
async function asleepCases() {
  const ws = (id, status) => ({ id, name: id, repoPath: "/r/" + id, status, panes: [{ id: "p-" + id }] });
  let reply = [ws("ws-on", "live"), ws("ws-off", "cold")];
  const revived = [];
  const r = run({
    workspacesReply: () => reply,
    onRevive: (u) => { revived.push(u); reply = reply.map((x) => (u.includes(x.id) ? { ...x, status: "live" } : x)); },
  });
  await r.ctx.fetchWorkspaces();
  await settle();
  await r.ctx.openWorkspace("ws-off", null);
  await settle();
  check("opening a sleeping session wakes it", revived.join() === "/v1/workspaces/ws-off/revive", revived.join() || "no wake");
  check("a woken session is dialed and shown", attachedTo(r).includes("ws-off") && r.evalIn("state.wsId") === "ws-off",
    `${attachedTo(r).join(" ")} / ${r.evalIn("state.wsId")}`);

  // ws-on falls asleep while shown (another lens, or a blip): its socket
  // closes and the reconnect fires before any list refresh says so.
  // A hello lands in the agent chat module (show or hide), which this page
  // does not load; a stand-in records nothing and draws nothing.
  r.ctx.ccmuxAgentChat = { show() {}, hide() {} };
  const hello = (sk) => sk.onmessage({ data: JSON.stringify({ t: "hello", panes: [{ id: "p1" }, { id: "p2" }] }) });
  await r.ctx.openWorkspace("ws-on", "p2");
  await settle();
  const sock = r.live[r.live.length - 1];
  hello(sock);
  r.evalIn(`wsById("ws-on").status = "cold"`);
  const dials = attachedTo(r).length;
  const wakes = revived.length;
  sock.onclose();
  r.fireTimers();
  await settle();
  check("a session that fell asleep is not dialed again", attachedTo(r).length === dials, attachedTo(r).join(" "));
  check("a session that fell asleep is not woken behind anyone's back", revived.length === wakes, revived.join(" "));
  check("a session that fell asleep stays the one on screen", r.evalIn("state.wsId") === "ws-on", String(r.evalIn("state.wsId")));
  r.fireTimers();
  await settle();
  check("a waiting session runs no retry loop", attachedTo(r).length === dials, attachedTo(r).join(" "));

  reply = [ws("ws-on", "live"), ws("ws-off", "live")];
  await r.ctx.fetchWorkspaces();
  await settle();
  check("a waiting session comes back by itself once awake", attachedTo(r).length === dials + 1 &&
    attachedTo(r)[attachedTo(r).length - 1] === "ws-on", attachedTo(r).join(" "));
  hello(r.live[r.live.length - 1]);
  check("a session that comes back is on the tab it was on", r.evalIn("state.paneId") === "p2", String(r.evalIn("state.paneId")));

  // The page restoring what it showed never wakes anything: a reload (also
  // the automatic one after a deploy), a relaunch, a window switch. Only a
  // deliberate open does, so a closed session stays closed.
  const winA = () => [{ id: "win-a", name: "A", workspaceIds: ["ws-x", "ws-y"], openBy: ["p"], open: true, openHere: true }];
  for (const [what, id, opts] of [
    ["a reload on a sleeping session", "ws-x", { search: "?w=win-a&ws=ws-x" }],
    ["a window whose last session is asleep", "ws-x", { search: "?w=win-a", storage: { "ccmux-last-ws:win-a": "ws-x" } }],
    ["a link to a sleeping session in no window", "ws-z", { search: "?ws=ws-z" }],
    ["a link read before the window list", "ws-x", { search: "?w=win-a&ws=ws-x", windowsOkFor: (n) => n > 0 }],
    ["a phone whose last session is asleep", "ws-x", { phone: true, lastSession: "ws-x" }],
    ["a phone whose last session is asleep and in no window", "ws-z", { phone: true, lastSession: "ws-z" }],
    ["a phone with no window open and its last session asleep", "ws-z", { phone: true, lastSession: "ws-z", windowsReply: () => [] }],
  ]) {
    const woken = [];
    const status = { "ws-x": "cold", "ws-y": "live", "ws-z": "cold" };
    const b = run({
      windowsReply: winA, workspacesReply: () => Object.keys(status).map((k) => ws(k, status[k])),
      onRevive: (u) => { woken.push(u); status[id] = "live"; }, ...opts,
    });
    await settle();
    check(`${what} does not wake it`, woken.length === 0, woken.join(" "));
    check(`${what} shows it asleep, not dialed`, b.evalIn("state.wsId") === id && b.evalIn("state.asleepWait") === true &&
      !attachedTo(b).includes(id), `${b.evalIn("state.wsId")} / ${attachedTo(b).join(" ")}; errors: ${b.errors.join(" | ")}`);
    if (woken.length) continue;
    // Its row, clicked on that same page, does wake it, and dials it once.
    await b.ctx.openRow(id);
    await settle();
    check(`${what}: its row wakes it`, woken.join() === `/v1/workspaces/${id}/revive`, woken.join(" ") || "no wake");
    check(`${what}: its row dials it once`, attachedTo(b).filter((x) => x === id).length === 1, attachedTo(b).join(" "));
  }


  // A wake that fails says so and leaves the session waiting, not dialed.
  const f = run({ workspacesReply: () => [ws("ws-off", "cold")], reviveFails: true });
  await f.ctx.fetchWorkspaces();
  await settle();
  await f.ctx.openWorkspace("ws-off", null);
  await settle();
  check("a failed wake says so, in the daemon's words", f.alerts.some((m) => m.includes("ws-off") && m.includes("host unreachable") &&
    !m.includes("{")), f.alerts.join(" | ") || "no alert");
  check("a failed wake does not dial", !attachedTo(f).includes("ws-off"), attachedTo(f).join(" "));

  // Waking an agent opens it, though the list still said asleep when the wake
  // was asked for: the daemon's answer, not the old list, decides.
  let agentReply = [ws("ws-on", "live"), ws("ws-agent", "cold")];
  const w = run({
    workspacesReply: () => agentReply,
    agentWake: () => { agentReply = [ws("ws-on", "live"), ws("ws-agent", "live")]; return { id: "p-ws-agent", workspaceId: "ws-agent" }; },
  });
  await w.ctx.fetchWorkspaces();
  await settle();
  await w.ctx.openWorkspace("ws-on", null);
  await w.ctx.wakeAndOpen("win-a", "helper", "");
  await settle();
  check("a woken agent is dialed", attachedTo(w).includes("ws-agent"), attachedTo(w).join(" "));
  check("a woken agent is the session on screen", w.evalIn("state.wsId") === "ws-agent", String(w.evalIn("state.wsId")));
}

// A closed window comes back as it was left (windowwake.js): the close says
// which sessions are awake, and the open after it wakes only what the daemon
// hands back.
async function windowWakeCases() {
  const win = { id: "win-a", name: "A", workspaceIds: ["ws-1", "ws-2", "ws-3"], openBy: [], open: false, openHere: false };
  const ws = (id, status) => ({ id, name: id, repoPath: "/r/" + id, status, panes: [] });
  const r = run({
    windowsReply: () => [win],
    workspacesReply: () => [ws("ws-1", "live"), ws("ws-2", "cold"), ws("ws-3", "cold")],
    openReply: { wake: ["ws-2"] },
  });
  await r.ctx.fetchWorkspaces();
  await settle();
  await r.ctx.closeWindow(win);
  const close = r.fetched.find((f) => f.method === "POST" && f.url.startsWith("/v1/windows/win-a/close?"));
  check("a window's close says which of its sessions are awake",
    !!close && JSON.parse(close.body).awake.join() === "ws-1", close ? String(close.body) : "no close sent");
  await r.ctx.openWindow(win);
  const woke = r.fetched.filter((f) => f.method === "POST" && f.url.includes("/revive")).map((f) => f.url);
  check("a reopened window wakes only what was awake at its last close",
    woke.join() === "/v1/workspaces/ws-2/revive", woke.join(" ") || "nothing woken");

  // An answer the lens cannot read wakes nothing: waking everything could
  // start sessions someone left asleep.
  const garbled = run({ windowsReply: () => [win], workspacesReply: () => [ws("ws-1", "live"), ws("ws-2", "cold")], openReply: "garbled" });
  await garbled.ctx.fetchWorkspaces();
  await settle();
  const garbledOpen = await garbled.ctx.openWindow(win);
  const garbledWoke = garbled.fetched.filter((f) => f.url.includes("/revive")).length;
  check("an unreadable open answer still opens the window, wakes nothing, and says so",
    garbledOpen === true && garbledWoke === 0 && garbled.alerts.length === 1,
    `open ${garbledOpen}, ${garbledWoke} woken, alerts ${JSON.stringify(garbled.alerts)}`);

  // A session that will not wake is that, not a failed open: the open landed
  // and the lens says which, once, and stays open.
  const stuck = run({
    windowsReply: () => [win],
    workspacesReply: () => [ws("ws-2", "cold"), ws("ws-3", "cold")],
    openReply: { wake: ["ws-2", "ws-3"] }, reviveFails: true,
  });
  await stuck.ctx.fetchWorkspaces();
  await settle();
  const stuckOpen = await stuck.ctx.openWindow(win);
  const tried = stuck.fetched.filter((f) => f.url.includes("/revive")).length;
  check("a session that will not wake leaves the window open, tries the rest, and says so once",
    stuckOpen === true && tried === 2 && stuck.alerts.length === 1 && /did not wake/.test(stuck.alerts[0] || ""),
    `open ${stuckOpen}, ${tried} tried, alerts ${JSON.stringify(stuck.alerts)}`);
  await windowWakeFailureCases(win, ws);
}

// A wake whose request fails outright (a dropped connection) is a session
// that did not wake, not a failed open; an open the daemon refuses is a
// failed open and wakes nothing.
async function windowWakeFailureCases(win, ws) {
  const dropped = run({
    windowsReply: () => [win],
    workspacesReply: () => [ws("ws-2", "cold"), ws("ws-3", "cold")],
    openReply: { wake: ["ws-2", "ws-3"] }, reviveThrows: ["ws-2"],
  });
  await dropped.ctx.fetchWorkspaces();
  await settle();
  const before = dropped.fetched.length;
  const droppedOpen = await dropped.ctx.openWindow(win);
  const after = dropped.fetched.slice(before);
  const tried = after.filter((f) => f.url.includes("/revive")).length;
  const reread = after.some((f) => f.url.startsWith("/v1/workspaces") && !f.url.includes("/revive"));
  check("a wake whose request fails leaves the window open, tries the rest, says which, and reads the list again",
    droppedOpen === true && tried === 2 && dropped.alerts.length === 1 && /ws-2/.test(dropped.alerts[0] || "") && reread,
    `open ${droppedOpen}, ${tried} tried, reread ${reread}, alerts ${JSON.stringify(dropped.alerts)}`);

  const denied = run({ windowsReply: () => [win], workspacesReply: () => [ws("ws-2", "cold")], openDenied: true });
  await denied.ctx.fetchWorkspaces();
  await settle();
  const deniedOpen = await denied.ctx.openWindow(win);
  const deniedWoke = denied.fetched.filter((f) => f.url.includes("/revive")).length;
  check("an open the daemon refuses is a failed open and wakes nothing",
    deniedOpen === false && deniedWoke === 0 && denied.alerts.length === 1 && /open failed/.test(denied.alerts[0] || ""),
    `open ${deniedOpen}, ${deniedWoke} woken, alerts ${JSON.stringify(denied.alerts)}`);
}

async function pageWindowCases() {
  const link = run({ windowsReply: twoWindows, search: "?ws=ws-2" });
  await settle();
  check("a notification link opens its session in the session's window",
    attachedTo(link).join() === "ws-2" && lastAddress(link) === "/?w=win-b&ws=ws-2",
    `attached ${attachedTo(link).join()}, address ${lastAddress(link)}; errors: ${link.errors.join(" | ")}`);

  // The map the service worker routes taps by (sw_test.js reads the same key).
  const mapPut = link.cachePuts.find(([k]) => k === "/__ccmux_ws_windows");
  check("pages publish which window each session is in, for the service worker",
    mapPut && mapPut[1] === JSON.stringify({ "ws-1": "win-a", "ws-2": "win-b" }), JSON.stringify(link.cachePuts));

  const back = run({ windowsReply: twoWindows, search: "?w=win-a", storage: { "ccmux-last-ws:win-a": "ws-1" } });
  await settle();
  check("a window shows the session last shown in it", attachedTo(back).join() === "ws-1",
    `attached ${attachedTo(back).join()}; errors: ${back.errors.join(" | ")}`);

  // A fresh launch with nothing remembered still brings back every window
  // open on this device, each in its own browser window, as the Mac app does.
  const fresh = run({ windowsReply: twoWindows });
  await settle();
  check("a launch with two windows open and nothing remembered opens both",
    fresh.ctx.ccmuxPage.current() === "win-a" && lastAddress(fresh).startsWith("/?w=win-a") &&
    fresh.opened.join() === "/?w=win-b",
    `current ${fresh.ctx.ccmuxPage.current()}, address ${lastAddress(fresh)}, opened ${fresh.opened.join()}`);
  await phoneCases();
  // A window open on this device but closed in the daemon's list for this
  // lens (openHere false) stays closed.
  const oneOpen = () => [twoWindows()[0], { ...twoWindows()[1], openHere: false, open: false }];
  const partial = run({ windowsReply: oneOpen });
  await settle();
  check("a launch opens only the windows open on this device",
    partial.ctx.ccmuxPage.current() === "win-a" && partial.opened.length === 0,
    `current ${partial.ctx.ccmuxPage.current()}, opened ${partial.opened.join()}`);

  // Right-click choices on a closed window, and "Open in new window" on an open one.
  const menus = run({ windowsReply: oneOpen, search: "?w=win-a" });
  await settle();
  const winB = menus.evalIn("state.windows").find((w) => w.id === "win-b");
  await menus.ctx.ccmuxPage.openClosed(winB, "");
  await settle();
  check("a plain click on a closed window, from a browser window showing another, opens a new browser window",
    menus.opened.join() === "/?w=win-b" && menus.ctx.ccmuxPage.current() === "win-a",
    `opened ${menus.opened.join()}, current ${menus.ctx.ccmuxPage.current()}`);
  menus.opened.length = 0;
  await menus.ctx.ccmuxPage.openClosed(winB, "new");
  await settle();
  check("Open in new window on a closed window opens a browser window and keeps this one",
    menus.opened.join() === "/?w=win-b" && menus.ctx.ccmuxPage.current() === "win-a",
    `opened ${menus.opened.join()}, current ${menus.ctx.ccmuxPage.current()}`);
  await menus.ctx.ccmuxPage.openClosed(winB, "here");
  await settle();
  check("Open here on a closed window moves this browser window to it",
    menus.opened.length === 1 && menus.ctx.ccmuxPage.current() === "win-b",
    `opened ${menus.opened.join()}, current ${menus.ctx.ccmuxPage.current()}`);
  menus.ctx.ccmuxPage.openElsewhere(menus.evalIn("state.windows").find((w) => w.id === "win-a"));
  check("Open in new window on an open window opens a browser window for it",
    menus.opened[menus.opened.length - 1] === "/?w=win-a" && menus.ctx.ccmuxPage.current() === "win-b",
    `opened ${menus.opened.join()}, current ${menus.ctx.ccmuxPage.current()}`);

  const quit = JSON.stringify({ p1: { w: "win-a", seen: 0, closedAt: 1000 }, p2: { w: "win-b", seen: 0, closedAt: 1001 } });
  const relaunch = run({ windowsReply: twoWindows, storage: { "ccmux-pages": quit } });
  await settle();
  check("a relaunch shows the first window here and asks the browser for the rest",
    lastAddress(relaunch).startsWith("/?w=win-a") && relaunch.opened.join() === "/?w=win-b",
    `address ${lastAddress(relaunch)}, opened ${relaunch.opened.join()}; errors: ${relaunch.errors.join(" | ")}`);

  const refused = run({ windowsReply: twoWindows, storage: { "ccmux-pages": quit }, openRefused: true });
  await settle();
  check("a browser that refuses the extra windows costs nothing but a button",
    lastAddress(refused).startsWith("/?w=win-a") && refused.opened.join() === "/?w=win-b" && refused.errors.length === 0,
    `opened ${refused.opened.join()}; errors: ${refused.errors.join(" | ")}`);

  // A cold launch from a notification tap: push.js attaches from the stashed
  // tap before app.js has read the window list, and must not wipe the address
  // the boot is about to read.
  const cold = run({ windowsReply: twoWindows, search: "?w=win-b&ws=ws-2", withPush: true,
    pendingNav: `/?w=win-b&ws=ws-2&t=${Date.now()}` });
  await settle();
  check("a notification that launches the app lands on its session, in its window",
    cold.ctx.ccmuxPage.current() === "win-b" && lastAddress(cold) === "/?w=win-b&ws=ws-2",
    `current ${cold.ctx.ccmuxPage.current()}, addresses ${cold.addresses.join(" ")}`);

  // Switching windows from a header takes down the old window's board: only
  // openWorkspace closed it before, and a window with no session to show
  // left Mixed's board on screen under a sidebar that said ChartLabs.
  const boarded = run({ windowsReply: twoWindows, search: "?w=win-a", withBoard: true });
  await settle();
  boarded.ctx.ccmuxBoard.open("Mixed");
  await boarded.ctx.ccmuxPage.enter("win-b", "");
  check("switching windows closes the other window's attention board",
    !boarded.ctx.ccmuxBoard.isOpen() && boarded.ctx.ccmuxPage.current() === "win-b",
    `board open: ${boarded.ctx.ccmuxBoard.isOpen()}, current ${boarded.ctx.ccmuxPage.current()}`);
  // Each window has its own task queue, so the old window's goes the same way.
  for (const f of ["tasklist.js", "tasks.js"]) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, f), "utf8"), boarded.ctx, { filename: f });
  }
  boarded.ctx.ccmuxTasks.open({ id: "win-b", name: "ChartLabs" });
  await boarded.ctx.ccmuxPage.enter("win-b", "");
  check("staying in a window keeps its task queue open", boarded.ctx.ccmuxTasks.openWindow() === "win-b");
  await boarded.ctx.ccmuxPage.enter("win-a", "");
  check("switching windows closes the other window's task queue", !boarded.ctx.ccmuxTasks.isOpen(),
    `tasks open for ${boarded.ctx.ccmuxTasks.openWindow()}`);

  // A window list that cannot be read at boot decides nothing: the tapped
  // session still opens, the address and the relaunch list stay as they were,
  // and the next good read finishes the boot.
  const blind = run({ windowsOk: false, search: "?w=win-b&ws=ws-2" });
  await settle();
  check("an unreadable window list still opens the tapped session and keeps the address",
    attachedTo(blind).join() === "ws-2" && blind.addresses.length === 0,
    `attached ${attachedTo(blind).join()}, addresses ${blind.addresses.join(" ")}`);
  const kept = { "ccmux-pages": quit };
  const blindRelaunch = run({ windowsOk: false, storage: kept });
  await settle();
  check("an unreadable window list keeps last session's windows for the next launch",
    kept["ccmux-pages"].includes("p1") && kept["ccmux-pages"].includes("p2") && blindRelaunch.opened.length === 0,
    kept["ccmux-pages"]);
  const late = run({ windowsReply: twoWindows, windowsOkFor: (n) => n > 0, search: "?w=win-b&ws=ws-2" });
  await settle();
  check("before a good read the page has no window", late.ctx.ccmuxPage.current() === "");
  await late.ctx.fetchWorkspaces();
  await settle();
  check("the first good read finishes the boot", late.ctx.ccmuxPage.current() === "win-b" &&
    lastAddress(late) === "/?w=win-b&ws=ws-2" && attachedTo(late).join() === "ws-2",
    `current ${late.ctx.ccmuxPage.current()}, address ${lastAddress(late)}, attached ${attachedTo(late).join()}`);

  // The session in a link decides: a window id from before the session moved
  // (or from a deleted window) must not cost the user the session they tapped.
  const moved = run({ windowsReply: twoWindows, search: "?w=win-a&ws=ws-2" });
  await settle();
  check("a link whose window is out of date still opens its session, in its window now",
    moved.ctx.ccmuxPage.current() === "win-b" && attachedTo(moved).join() === "ws-2",
    `current ${moved.ctx.ccmuxPage.current()}, attached ${attachedTo(moved).join()}`);

  // The window that takes a tap by message spends the stash; another window
  // leaves a tap meant for a window it does not show.
  const owner = run({ windowsReply: twoWindows, search: "?w=win-b", withPush: true });
  await settle();
  for (const fn of owner.swListeners) fn({ data: { type: "ccmux-navigate", url: "/?w=win-b&ws=ws-2" } });
  await settle();
  check("the window that takes a tap by message clears the stash",
    owner.cacheDeletes.includes("/__ccmux_pending_nav") && attachedTo(owner).includes("ws-2"),
    `deletes ${owner.cacheDeletes.join()}, attached ${attachedTo(owner).join()}`);
  // Only a missed message leaves a stash to read, and the window that reads it
  // is the one the tap brought back: an iPhone's single window showing Mixed
  // must still open a ChartLabs session it was tapped for.
  const missed = run({ windowsReply: twoWindows, search: "?w=win-a", withPush: true,
    pendingNav: `/?w=win-b&ws=ws-2&t=${Date.now()}` });
  await settle();
  check("a window that missed the message still opens the tapped session, in its window",
    missed.evalIn("state.wsId") === "ws-2" && missed.ctx.ccmuxPage.current() === "win-b",
    `attached ${attachedTo(missed).join()}, current ${missed.ctx.ccmuxPage.current()}`);
  const stale = run({ windowsReply: twoWindows, search: "?w=win-a", withPush: true,
    pendingNav: `/?w=win-b&ws=ws-2&t=${Date.now() - 61000}` });
  await settle();
  check("a tap nobody took for a minute is dropped, not replayed",
    !attachedTo(stale).includes("ws-2") && stale.cacheDeletes.includes("/__ccmux_pending_nav") &&
    stale.ctx.ccmuxPage.current() === "win-a", `attached ${attachedTo(stale).join()}, deletes ${stale.cacheDeletes.join()}`);

  // A live session in no window (AVAILABLE) is shown inside the window this
  // browser window already has, and stays in the address, so a reload, and a
  // second one, land in the same place.
  const sessions = () => [{ id: "ws-1" }, { id: "ws-2" }, { id: "loose" }];
  const looseStore = {};
  const avail = run({ windowsReply: twoWindows, workspacesReply: sessions, search: "?w=win-a&ws=loose",
    storage: looseStore, name: "ccmux-page:loose" });
  await settle();
  check("a session in no window shows inside this browser window's window",
    avail.evalIn("state.wsId") === "loose" && avail.ctx.ccmuxPage.current() === "win-a" &&
    lastAddress(avail) === "/?w=win-a&ws=loose",
    `on ${avail.evalIn("state.wsId")}, current ${avail.ctx.ccmuxPage.current()}, address ${lastAddress(avail)}`);
  let again = avail;
  for (let i = 0; i < 2; i++) {
    const search = lastAddress(again).slice(1);
    again = run({ windowsReply: twoWindows, workspacesReply: sessions, search, storage: looseStore, name: "ccmux-page:loose" });
    await settle();
  }
  check("reloading twice on a session in no window keeps the window and the session",
    again.ctx.ccmuxPage.current() === "win-a" && again.evalIn("state.wsId") === "loose" &&
    lastAddress(again) === "/?w=win-a&ws=loose",
    `current ${again.ctx.ccmuxPage.current()}, on ${again.evalIn("state.wsId")}, address ${lastAddress(again)}`);
  const looseTap = run({ windowsReply: twoWindows, workspacesReply: sessions, search: "?w=win-a", withPush: true,
    pendingNav: `/?ws=loose&t=${Date.now()}`, storage: { "ccmux-last-ws:win-a": "ws-1" } });
  await settle();
  check("a tap for a session in no window, read while the page starts, is not undone",
    looseTap.evalIn("state.wsId") === "loose" && looseTap.ctx.ccmuxPage.current() === "win-a" &&
    lastAddress(looseTap) === "/?w=win-a&ws=loose" && !attachedTo(looseTap).includes("ws-1"),
    `on ${looseTap.evalIn("state.wsId")}, current ${looseTap.ctx.ccmuxPage.current()}, address ${lastAddress(looseTap)}, attached ${attachedTo(looseTap).join()}`);
  // With no window in the address it shows on its own, and a reload keeps it.
  const alone = run({ windowsReply: twoWindows, workspacesReply: sessions, search: "?ws=loose" });
  await settle();
  check("a session in no window with no window in the address shows alone and stays in the address",
    alone.evalIn("state.wsId") === "loose" && alone.ctx.ccmuxPage.current() === "" && lastAddress(alone) === "/?ws=loose",
    `on ${alone.evalIn("state.wsId")}, current ${alone.ctx.ccmuxPage.current()}, address ${lastAddress(alone)}`);

  // A window whose session was deleted keeps its window on a reload or restore.
  const orphan = run({ windowsReply: twoWindows, search: "?w=win-a&ws=gone" });
  await settle();
  check("an address whose session is gone keeps its window",
    orphan.ctx.ccmuxPage.current() === "win-a" && !attachedTo(orphan).includes("gone") && lastAddress(orphan) === "/?w=win-a",
    `current ${orphan.ctx.ccmuxPage.current()}, attached ${attachedTo(orphan).join()}, address ${lastAddress(orphan)}`);
  const orphanBack = run({ windowsReply: twoWindows, workspacesReply: sessions, search: "?w=win-a&ws=gone",
    storage: { "ccmux-last-ws:win-a": "ws-1" } });
  await settle();
  check("an address whose session is gone shows the window's remembered session",
    orphanBack.ctx.ccmuxPage.current() === "win-a" && orphanBack.evalIn("state.wsId") === "ws-1" &&
    lastAddress(orphanBack) === "/?w=win-a&ws=ws-1",
    `current ${orphanBack.ctx.ccmuxPage.current()}, on ${orphanBack.evalIn("state.wsId")}, address ${lastAddress(orphanBack)}`);

  // A reload of the picker is not a relaunch: windows closed earlier stay closed.
  const reloadQuit = JSON.stringify({ me: { w: "", seen: 0, closedAt: 2000 }, p1: { w: "win-a", seen: 0, closedAt: 1000 } });
  const reload = run({ windowsReply: twoWindows, name: "ccmux-page:me", storage: { "ccmux-pages": reloadQuit } });
  await settle();
  check("reloading the picker reopens nothing", reload.opened.length === 0 && reload.ctx.ccmuxPage.current() === "" &&
    attachedTo(reload).length === 0, `current ${reload.ctx.ccmuxPage.current()}, opened ${reload.opened.join()}`);

  await cmdNCase();
  await handOffCases();

  const follow = run({ windowsReply: twoWindows, search: "?w=win-a" });
  await settle();
  follow.ctx.openWorkspace("ws-2", null);
  await settle();
  check("opening another window's session moves this browser window there",
    lastAddress(follow) === "/?w=win-b&ws=ws-2" && follow.ctx.ccmuxPage.current() === "win-b", lastAddress(follow));
}

// phoneCases: a phone is a support lens. It mirrors what its person has open
// on their computers, holds nothing open by looking, and starts in the session
// they last typed in.
async function phoneCases() {
  // Open for the person on a computer, not on this phone: still listed open.
  const onComputer = () => twoWindows().map((w) => ({ ...w, openHere: false }));
  const posts = (r, path) => r.fetched.filter((f) => f.method === "POST" && f.url.includes(path));

  const plain = run({ windowsReply: onComputer, phone: true });
  await settle();
  check("a phone with no last session starts in the first window open for its person, and opens no others",
    plain.ctx.ccmuxPage.current() === "win-a" && plain.opened.length === 0,
    `current ${plain.ctx.ccmuxPage.current()}, opened ${plain.opened.join()}`);
  check("a phone holds nothing open by looking: no open flag is written for a window open on the computer",
    posts(plain, "/open?").length === 0, posts(plain, "/open?").map((f) => f.url).join(" "));
  const leftovers = posts(plain, "/v1/windows/open-set").map((f) => JSON.parse(f.body));
  check("a phone clears the open flags it set under its plain device id, and only those",
    leftovers.length === 1 && !leftovers[0].device.startsWith("phone:") && leftovers[0].windowIds.length === 0,
    JSON.stringify(leftovers));

  const live = () => [{ id: "ws-1" }, { id: "ws-2" }];
  const last = run({ windowsReply: onComputer, workspacesReply: live, phone: true, lastSession: "ws-2" });
  await settle();
  check("a phone starts in the session its person last typed in, inside that session's window",
    last.ctx.ccmuxPage.current() === "win-b" && attachedTo(last).join() === "ws-2",
    `current ${last.ctx.ccmuxPage.current()}, attached ${attachedTo(last).join()}`);
  // The last session's window was closed on the computer: the phone neither
  // reopens it nor wakes its sessions; it starts in a window that is open.
  const shut = () => [onComputer()[0], { ...twoWindows()[1], open: false, openHere: false }];
  const closedHome = run({ windowsReply: shut, workspacesReply: live, phone: true, lastSession: "ws-2" });
  await settle();
  check("a last session whose window was closed on the computer does not reopen it",
    closedHome.ctx.ccmuxPage.current() === "win-a" && posts(closedHome, "/open?").length === 0 &&
    posts(closedHome, "/revive").length === 0 && !attachedTo(closedHome).includes("ws-2"),
    `current ${closedHome.ctx.ccmuxPage.current()}, opens ${posts(closedHome, "/open?").length}, attached ${attachedTo(closedHome).join()}`);
  // Nothing open for the person at all: a last session whose window is closed
  // is not shown (the computer put it away); the picker is.
  const allShut = () => twoWindows().map((w) => ({ ...w, open: false, openHere: false }));
  const nothingOpen = run({ windowsReply: allShut, workspacesReply: live, phone: true, lastSession: "ws-2" });
  await settle();
  check("with nothing open, a phone does not show a last session whose window was closed",
    nothingOpen.ctx.ccmuxPage.current() === "" && !attachedTo(nothingOpen).includes("ws-2") &&
    posts(nothingOpen, "/open?").length === 0,
    `current ${nothingOpen.ctx.ccmuxPage.current()}, attached ${attachedTo(nothingOpen).join()}`);
  // A last session in no window, with nothing open: shown on its own.
  const loose = () => [{ id: "ws-1" }, { id: "ws-2" }, { id: "loose" }];
  const looseLast = run({ windowsReply: allShut, workspacesReply: loose, phone: true, lastSession: "loose" });
  await settle();
  check("with nothing open, a phone shows a last session that is in no window",
    attachedTo(looseLast).join() === "loose", `attached ${attachedTo(looseLast).join()}`);
  // The phone says who it is, as attach does.
  const asks = last.fetched.filter((f) => f.url.startsWith("/v1/me/last-session"));
  check("a phone asks for its last session under the name it types as",
    asks.length === 1 && asks[0].url.includes("?user="), asks.map((f) => f.url).join(" "));
  const gone = run({ windowsReply: onComputer, workspacesReply: live, phone: true, lastSession: "deleted" });
  await settle();
  check("a last session that no longer exists falls back to the first open window",
    gone.ctx.ccmuxPage.current() === "win-a" && !attachedTo(gone).includes("deleted"),
    `current ${gone.ctx.ccmuxPage.current()}, attached ${attachedTo(gone).join()}`);

  // A window closed everywhere, opened from the phone, is opened for the person
  // under the phone's own device, which the computer's close clears.
  const closedAll = () => [onComputer()[0], { ...twoWindows()[1], open: false, openHere: false }];
  const opener = run({ windowsReply: closedAll, phone: true });
  await settle();
  await opener.ctx.ccmuxPage.openClosed(opener.evalIn("state.windows").find((w) => w.id === "win-b"), "here");
  await settle();
  // A plain tap on a closed window, with one already showing, lands here on a
  // phone: a new browser window would leave the installed app.
  const tapper = run({ windowsReply: closedAll, phone: true });
  await settle();
  await tapper.ctx.ccmuxPage.openClosed(tapper.evalIn("state.windows").find((w) => w.id === "win-b"), "");
  await settle();
  check("a tap on a closed window opens it in place on a phone, never in a new browser window",
    tapper.opened.length === 0 && tapper.ctx.ccmuxPage.current() === "win-b",
    `opened ${tapper.opened.join()}, current ${tapper.ctx.ccmuxPage.current()}`);
  const opens = posts(opener, "/v1/windows/win-b/open?");
  check("opening a window from the phone records it under the phone's device",
    opens.length >= 1 && opens.every((f) => f.url.includes("device=phone%3A")) && opener.opened.length === 0,
    opens.map((f) => f.url).join(" ") + " opened " + opener.opened.join());
}

// handOffCases: a click on another window's session, while a browser window
// already shows that window, goes to that browser window instead of moving
// this one. Reached by reference when linked by opener (and raised), over the
// channel when opened by hand; nobody showing it keeps the old behavior.
function sharedChannel() {
  const members = [];
  return function BC() {
    const me = { onmessage: null, postMessage: (data) => {
      for (const m of members) if (m !== me && m.onmessage) setImmediate(() => m.onmessage({ data }));
    } };
    members.push(me);
    return me;
  };
}

async function handOffCases() {
  const mixed = run({ windowsReply: twoWindows, search: "?w=win-a" });
  await settle();
  const chart = run({ windowsReply: twoWindows, search: "?w=win-b", opener: mixed.ctx });
  await settle();
  check("a window opened by another joins its opener's list",
    mixed.ctx.ccmuxPage.kids().some((k) => k.ccmuxPage === chart.ctx.ccmuxPage), String(mixed.ctx.ccmuxPage.kids().length));
  await mixed.ctx.clickRow("ws-2");
  await settle();
  check("a click on another window's session goes to the linked browser window showing it, and raises it",
    attachedTo(chart).includes("ws-2") && chart.focused() === 1 &&
    mixed.ctx.ccmuxPage.current() === "win-a" && !attachedTo(mixed).includes("ws-2"),
    `chart attached ${attachedTo(chart).join()}, focused ${chart.focused()}, mixed on ${mixed.ctx.ccmuxPage.current()}`);
  const before = chart.focused();
  check("a header click raises it too", (await chart.ctx.ccmuxPage.handOff("win-a", "")) === true &&
    mixed.focused() === 1 && chart.focused() === before);

  const BC = sharedChannel();
  const shared = {};
  const a = run({ windowsReply: twoWindows, search: "?w=win-a", storage: shared, BroadcastChannel: BC });
  await settle();
  const b = run({ windowsReply: twoWindows, search: "?w=win-b", storage: shared, BroadcastChannel: BC });
  await settle();
  const bystander = run({ windowsReply: twoWindows, search: "?w=win-a", storage: shared, BroadcastChannel: BC });
  await settle();
  // Raced against a settle: timers are fake here, so a reply that never came
  // would hang the run instead of failing this check.
  const handed = await Promise.race([a.ctx.ccmuxPage.handOff("", "ws-2"), settle().then(() => "no answer")]);
  await settle();
  check("a browser window opened by hand still takes the click over the channel",
    handed === true && attachedTo(b).includes("ws-2") && a.ctx.ccmuxPage.current() === "win-a" && !attachedTo(a).includes("ws-2"),
    `handed ${handed}, b attached ${attachedTo(b).join()}, a on ${a.ctx.ccmuxPage.current()}, a attached ${attachedTo(a).join()}`);
  check("only the browser window showing that window takes it",
    !attachedTo(bystander).includes("ws-2") && bystander.ctx.ccmuxPage.current() === "win-a",
    `bystander attached ${attachedTo(bystander).join()}, on ${bystander.ctx.ccmuxPage.current()}`);

  // A crashed browser window never said goodbye, so storage still says it
  // shows win-b. Nobody answers; once the wait is over the click opens here.
  const ghost = { "ccmux-pages": JSON.stringify({ gone: { w: "win-b", seen: 1, closedAt: 0 } }) };
  const lone = run({ windowsReply: twoWindows, search: "?w=win-a", storage: ghost, BroadcastChannel: sharedChannel() });
  await settle();
  const click = lone.ctx.clickRow("ws-2");
  await settle();
  lone.fireTimers();
  await Promise.race([click, settle()]);
  await settle();
  check("a stale record of a window nobody shows still opens the click here after the wait",
    lone.ctx.ccmuxPage.current() === "win-b" && attachedTo(lone).includes("ws-2"),
    `current ${lone.ctx.ccmuxPage.current()}, attached ${attachedTo(lone).join()}`);

  const alone = run({ windowsReply: twoWindows, search: "?w=win-a" });
  await settle();
  await alone.ctx.clickRow("ws-2");
  await settle();
  check("with no browser window showing it, the click opens here as before",
    alone.ctx.ccmuxPage.current() === "win-b" && attachedTo(alone).includes("ws-2"),
    `current ${alone.ctx.ccmuxPage.current()}, attached ${attachedTo(alone).join()}`);
}

// cmdNCase: Cmd+N while another window is open. The two pages share storage
// and a BroadcastChannel; the open one answers the new one's ping, so the new
// one shows the picker, opens nothing, and forgets nothing.
async function cmdNCase() {
  const members = [];
  function BC() {
    const me = { onmessage: null, postMessage: (data) => {
      for (const m of members) if (m !== me && m.onmessage) setImmediate(() => m.onmessage({ data }));
    } };
    members.push(me);
    return me;
  }
  const shared = {};
  const first = run({ windowsReply: twoWindows, search: "?w=win-a", storage: shared, BroadcastChannel: BC });
  await settle();
  const second = run({ windowsReply: twoWindows, storage: shared, BroadcastChannel: BC });
  await settle();
  second.fireTimers(); // the ping's wait is over; the pong has long arrived
  await settle();
  const pages = JSON.parse(shared["ccmux-pages"] || "{}");
  const firstKept = Object.values(pages).some((p) => p.w === "win-a");
  check("Cmd+N while a window is open shows the picker and restores nothing",
    second.opened.length === 0 && second.ctx.ccmuxPage.current() === "" && firstKept,
    `opened ${second.opened.join()}, current ${second.ctx.ccmuxPage.current()}, pages ${shared["ccmux-pages"]}`);
  check("the open window is untouched by the new one", first.ctx.ccmuxPage.current() === "win-a");
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

// --- board.js: an agent tile drawn as its chat, whose cards answer in
// place (paintChat, answerCard). The Mac's BoardChatTileView keeps the same
// rules. Its own tiny DOM: the cards need class lookups, clicks and
// disabled buttons, which the permissive Proxy above cannot answer.
class TileEl {
  constructor(tag) { Object.assign(this, { tag, children: [], textContent: "", disabled: false, value: "", onclick: null, _cls: new Set() }); }
  set className(v) { this._cls = new Set(String(v || "").split(/\s+/).filter(Boolean)); }
  get className() { return [...this._cls].join(" "); }
  get classList() {
    const c = this._cls;
    return { add: (k) => c.add(k), remove: (k) => c.delete(k), contains: (k) => c.has(k),
      toggle: (k, on) => ((on === undefined ? !c.has(k) : on) ? c.add(k) : c.delete(k)) };
  }
  set innerHTML(v) { if (v === "") this.children = []; }
  get childElementCount() { return this.children.length; }
  appendChild(c) { this.children.push(c); c.parent = this; return c; }
  addEventListener(kind, fn) { this["on" + kind] = fn; }
  all() { return this.children.flatMap((c) => [c, ...c.all()]); }
  querySelectorAll(sel) { return this.all().filter((e) => (sel.startsWith(".") ? e._cls.has(sel.slice(1)) : e.tag === sel)); }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  // A click bubbles to the first ancestor with a handler that does not stop it.
  click() {
    let stopped = false;
    const ev = { stopPropagation: () => { stopped = true; } };
    for (let e = this; e && !stopped; e = e.parent) if (e.onclick) e.onclick(ev);
  }
}

async function boardChatTileCases() {
  const z = run({ withBoard: true });
  await tick();
  const page = z.ctx.document;
  z.ctx.document = new Proxy({}, { get: (_t, k) => (k === "createElement" ? (tag) => new TileEl(tag) : page[k]) });
  const Z = z.ctx.ccmuxBoard;
  const hello = { t: "hello", agent: "radar", state: "running",
    turns: [{ id: "m1", role: "assistant", parts: [{ id: "p1", type: "text", text: "Two questions." }] }],
    permissions: [{ id: "per_1", permission: "bash", patterns: ["ls"], always: [] }],
    questions: [{ id: "que_1", questions: [{ question: "VIX gap?", options: [{ label: "Say it plainly" }, { label: "Leave it alone" }] }] }] };
  const posts = [];
  let reply = { status: 200, body: '{"ok":true}' };
  z.ctx.fetch = async (u, o) => {
    posts.push({ url: String(u), body: JSON.parse(o.body) });
    return { ok: reply.status === 200, status: reply.status, json: async () => JSON.parse(reply.body) };
  };
  const draw = () => {
    const tile = new TileEl("div");
    let opened = 0;
    tile.onclick = () => { opened++; };
    const box = tile.appendChild(new TileEl("div"));
    Z.paintChat("q", box, hello);
    const button = (label) => box.querySelectorAll("button").find((b) => b.textContent === label ||
      (b.children[0] && b.children[0].textContent === label));
    return { box, button, opened: () => opened };
  };

  const t = draw();
  check("board chat tile: draws the turns and every waiting card",
    t.box.querySelectorAll(".ac-turn").length === 1 && t.box.querySelectorAll(".ac-perm").length === 2 &&
    !t.button("Allow always"), t.box.querySelectorAll(".ac-perm").length);
  t.button("Say it plainly").click();
  await tick();
  t.button("Allow once").click();
  await tick();
  check("board chat tile: a card sends its answer, naming its request, over the reply route",
    posts.length === 2 && posts.every((p) => p.url === "/v1/panes/q/agent/reply") &&
    JSON.stringify(posts.map((p) => p.body)) ===
      '[{"t":"question","id":"que_1","answers":[["Say it plainly"]]},{"t":"permission","id":"per_1","reply":"once"}]',
    JSON.stringify(posts));
  check("board chat tile: a card tap answers, it does not open the tile", t.opened() === 0, t.opened());
  check("board chat tile: a sent answer keeps the cards off until the next read",
    t.box.querySelectorAll("button").every((b) => b.disabled));
  t.box.querySelector(".ac-turn").click();
  check("board chat tile: a tap on the turns opens the tile", t.opened() === 1, t.opened());

  reply = { status: 503, body: '{"error":"the agent\'s server did not answer"}' };
  const f = draw();
  f.button("Reject").click();
  await tick();
  await tick();
  const err = f.box.querySelector(".ac-error");
  check("board chat tile: a failed answer says why and turns the cards back on",
    !err.classList.contains("hidden") && err.textContent === "Not sent: the agent's server did not answer" &&
    f.box.querySelectorAll("button").every((b) => !b.disabled), err.textContent);
  reply = { status: 404, body: "not json" };
  const n = draw();
  n.button("Reject").click();
  await tick();
  await tick();
  check("board chat tile: a reply with no error body names its status",
    n.box.querySelector(".ac-error").textContent === "Not sent: HTTP 404", n.box.querySelector(".ac-error").textContent);
}

// --- owns.js: the "What It Owns…" editor (the Mac's OwnsSheetView keeps the
// same rules). A failed read must not let Save overwrite a typed line with
// the empty box; a good read fills the box and Save sends the line.
async function ownsEditorCases() {
  const z = run({});
  await tick();
  const els = {};
  const page = z.ctx.document;
  z.ctx.document = new Proxy({}, { get: (_t, k) => (k === "getElementById" ? (id) => els[id] || (els[id] = new TileEl("div")) : page[k]) });
  vm.runInContext(fs.readFileSync(path.join(__dirname, "owns.js"), "utf8"), z.ctx, { filename: "owns.js" });
  const O = z.ctx.ccmuxOwns;
  const puts = [];
  let read = { status: 503, body: { error: "owns lines are not configured on this daemon" } };
  z.ctx.fetch = async (u, o) => {
    const method = (o && o.method) || "GET";
    if (method === "PUT") puts.push(JSON.parse(o.body));
    const r = method === "PUT" ? { status: 200, body: { auto: "", manual: puts[puts.length - 1].manual, line: "" } } : read;
    return { ok: r.status === 200, status: r.status, json: async () => r.body };
  };

  await O.open({ id: "w1", name: "backend" });
  await O.save("");
  check("owns editor: after a failed read, Save sends nothing and the two writing buttons are off",
    puts.length === 0 && els["owns-auto"].textContent.startsWith("Could not read") &&
      els["owns-save"].disabled && els["owns-clear"].disabled && !els["owns-close"].disabled, JSON.stringify(puts));

  read = { status: 200, body: { auto: "API, database", manual: "API, auth, billing", line: "API, auth, billing" } };
  await O.open({ id: "w1", name: "backend" });
  check("owns editor: a good read fills the box with the typed line and shows the summary",
    els["owns-manual"].value === "API, auth, billing" && els["owns-auto"].textContent === "Summary: API, database" &&
      !els["owns-save"].disabled && !els["owns-clear"].disabled,
    els["owns-manual"].value + " | " + els["owns-auto"].textContent);
  await O.save("API, auth");
  check("owns editor: Save sends the line and closes",
    JSON.stringify(puts) === '[{"manual":"API, auth"}]' && els["owns-modal"].classList.contains("hidden"), JSON.stringify(puts));
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
// The peer messages socket waits longer after each failure too (retry.js).
async function peersRetryCases() {
  const s = runPeers();
  s.open("Mixed");
  await settle();
  const waits = [];
  for (let i = 0; i < 4; i++) {
    s.drop();
    waits.push(s.delays[s.delays.length - 1]);
    s.timers.splice(0).forEach((f) => f());
    await settle();
  }
  s.connected();
  s.drop();
  waits.push(s.delays[s.delays.length - 1]);
  check("peer messages wait longer after each failure, and start over once connected",
    waits.join() === "2000,4000,8000,16000,2000", waits.join());

  // Each opening counts its own failures: a run of them in one group does
  // not make the next group's first redial wait long.
  s.drop(); s.timers.splice(0).forEach((f) => f()); await settle();
  s.drop(); s.timers.splice(0).forEach((f) => f()); await settle();
  s.open("ChartLabs");
  await settle();
  s.drop();
  check("another group's messages start at 2s", s.delays[s.delays.length - 1] === 2000, s.delays.slice(-3).join());
}

function runPeers() {
  const els = {};
  const sockets = [];
  const timers = [];
  const delays = [];
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
    setTimeout: (f, ms) => { timers.push(f); delays.push(ms); return f; },
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
  for (const f of ["retry.js", "peers.js"]) vm.runInContext(fs.readFileSync(path.join(__dirname, f), "utf8"), ctx, { filename: f });
  const sock = () => sockets[sockets.length - 1];
  return {
    replies, timers, delays,
    drop: () => sock().onclose(),
    connected: () => sock().onopen(),
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

// A connection that keeps failing waits longer before each redial (retry.js):
// 2s doubling to 30s, and 2s again once it connects. Every socket used to
// redial every 2s, and Chrome then held back the page's other connections.
// redials drops a socket n times, firing its redial each time, and returns
// the wait each drop asked for; then it connects and drops once more.
async function redials(r, part, n) {
  const last = () => r.live[r.sockets.map((u, i) => [u, i]).filter(([u]) => u.includes(part)).pop()[1]];
  const waits = [];
  for (let i = 0; i < n; i++) {
    last().onclose();
    waits.push(r.delays[r.delays.length - 1]);
    r.fireTimers();
    await settle();
  }
  last().onopen();
  last().onclose();
  waits.push(r.delays[r.delays.length - 1]);
  return waits;
}

async function retryCases() {
  const want = "2000,4000,8000,16000,2000";
  const ws = (id) => ({ id, name: id, repoPath: "/r/" + id, status: "live", panes: [{ id: "p-" + id }] });
  const r = run({ workspacesReply: () => [ws("ws-a")], withBoard: true });
  await r.ctx.fetchWorkspaces();
  await settle();
  await r.ctx.openWorkspace("ws-a", null);
  await settle();
  r.ctx.ccmuxAgentChat = r.ctx.ccmuxAgentChat || { show() {}, hide() {} };
  // The terminal does not back off: while it is down, typing goes nowhere.
  const term = await redials(r, "/v1/attach", 4);
  check("the terminal keeps redialing every 2s", term.join() === "2000,2000,2000,2000,2000", term.join());
  const feed = await redials(r, "/v1/events", 4);
  check("the sidebar feed waits longer after each failure, and starts over once connected", feed.join() === want, feed.join());
  r.ctx.ccmuxAgentChat.show({ id: "p-ag", agent: "helper" }, ws("ws-ag"));
  const chat = await redials(r, "/agent/ws", 4);
  check("an agent's chat waits longer after each failure, and starts over once connected", chat.join() === want, chat.join());

  // Each chat counts its own failures: a run of them on one agent's chat
  // does not make the next agent's first redial wait long. Showing the same
  // chat again is not a new connection, so its count goes on.
  const lastChat = () => r.live[r.sockets.map((u, i) => [u, i]).filter(([u]) => u.includes("/agent/ws")).pop()[1]];
  const dropChat = () => { lastChat().onclose(); const d = r.delays[r.delays.length - 1]; r.fireTimers(); return d; };
  r.ctx.ccmuxAgentChat.show({ id: "p-ag2", agent: "other" }, ws("ws-ag2"));
  dropChat(); dropChat();
  r.ctx.ccmuxAgentChat.show({ id: "p-ag2", agent: "other" }, ws("ws-ag2"));
  check("showing the same chat again keeps its count", dropChat() === 8000, r.delays.slice(-3).join());
  r.ctx.ccmuxAgentChat.show({ id: "p-ag3", agent: "third" }, ws("ws-ag3"));
  check("another agent's chat starts at 2s", dropChat() === 2000, r.delays.slice(-3).join());
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

  // The daemon's claudeUpdate marks (restartMark in Go): each has its own
  // words, and no mark has none.
  const tips = ["pending", "manual"].map((m) => ok.ctx.updateTip(m));
  check("each update mark explains itself", tips.every((t) => t.startsWith("Claude update installed")) && tips[0] !== tips[1]);
  check("no mark, no tip", ok.ctx.updateTip(undefined) === "" && ok.ctx.updateTip("") === "" &&
    ok.ctx.updateTip("failed") === "");

  await closeTabCases();
  await scratchpadCases();
  await scratchDraftCases();
  await firehoseCases();
  await tasksCases();
  await emptyWindowCases();
  await scratchTabCases();
  await pageWindowCases();
  await windowWakeCases();
  attachOriginCases();
  await asleepCases();
  await peersMergeCases();
  await peersFailureCases();
  await retryCases();
  await peersRetryCases();
  await boardCases();
  await boardChatTileCases();
  await ownsEditorCases();

  console.log(failures === 0 ? "web lens smoke: ok" : `web lens smoke: ${failures} failure(s)`);
  process.exit(failures === 0 ? 0 : 1);
})();
