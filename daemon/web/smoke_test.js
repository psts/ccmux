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
const el = () => new Proxy(function () {}, {
  get: (_t, k) => (k === "children" || k === "childNodes" ? [] : k === "length" ? 0 : el()),
  set: () => true,
  apply: () => el(),
});

// windowsReply lets a case control each /v1/windows answer in turn, and gate
// lets it hold one open so two reads can resolve OUT OF ORDER.
function run({ windowsOk = true, windowsReply = null, gate = null } = {}) {
  const fetched = [];
  const storage = {};
  let reads = 0;
  const ctx = {
    console: { log() {}, warn() {}, error() {}, info() {} },
    document: new Proxy({}, { get: () => el() }),
    localStorage: {
      getItem: (k) => (k in storage ? storage[k] : null),
      setItem: (k, v) => { storage[k] = String(v); },
      removeItem: (k) => { delete storage[k]; },
    },
    crypto: { randomUUID: () => "test-device-uuid" },
    location: { href: "http://x/", search: "", pathname: "/", protocol: "http:", host: "x" },
    navigator: { userAgent: "node", serviceWorker: { register: () => Promise.resolve() } },
    WebSocket: function () { return el(); },
    setInterval: () => 0, setTimeout: () => 0, clearInterval() {}, clearTimeout() {},
    addEventListener() {}, matchMedia: () => ({ matches: false, addEventListener() {} }),
    prompt: () => "Patric", alert() {}, confirm: () => true,
    Notification: { permission: "default" },
    URLSearchParams, URL, TextEncoder, TextDecoder,
    atob: (x) => x, btoa: (x) => x,
    fetch: async (url, opts) => {
      const u = String(url);
      fetched.push({ url: u, method: (opts && opts.method) || "GET", body: opts && opts.body });
      if (u.startsWith("/v1/workspaces")) return { ok: true, status: 200, json: async () => [] };
      if (u.startsWith("/v1/windows?")) {
        if (!windowsOk) return { ok: false, status: 503, text: async () => "unreadable" };
        const n = reads++;
        const hold = gate ? gate(n) : null;
        return { ok: true, status: 200, json: async () => {
          if (hold) await hold;
          return windowsReply
            ? windowsReply(n)
            : [{ id: "win-here", name: "Here", workspaceIds: [], openBy: ["p"], open: true, openHere: true }];
        } };
      }
      return { ok: true, status: 200, text: async () => "", json: async () => ({}) };
    },
  };
  ctx.window = ctx; ctx.globalThis = ctx; ctx.self = ctx;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, "app.js"), "utf8"), ctx, { filename: "app.js" });
  // `state` is a top-level const, so it is a lexical binding and never lands on
  // the context object; only function declarations do. Later scripts in the same
  // context DO see it.
  const evalIn = (code) => vm.runInContext(code, ctx);
  return { ctx, fetched, evalIn };
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
  await acted.ctx.openWindow(win);
  await tick();
  await acted.ctx.closeWindow(win);
  await tick();
  for (const verb of ["open", "close"]) {
    const call = acted.fetched.find((f) => f.method === "POST" && f.url.includes(`/${verb}?`));
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

  // 6. A failed window read keeps the previous list rather than blanking it:
  //    every window would render as closed and feed wrong close decisions.
  const bad = run({ windowsOk: false });
  await bad.ctx.fetchWorkspaces();
  await tick();
  check("an unreadable window list does not blank state",
    Array.isArray(bad.evalIn("state.windows")),
    "state.windows is not an array after a 503");

  console.log(failures === 0 ? "web lens smoke: ok" : `web lens smoke: ${failures} failure(s)`);
  process.exit(failures === 0 ? 0 : 1);
})();
