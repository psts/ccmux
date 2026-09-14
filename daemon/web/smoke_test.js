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
function run({ windowsOk = true, windowsOkFor = null, windowsReply = null, gate = null, whoami = null, storage = {},
  whoamiGate = null, withPush = false, pendingNav = null } = {}) {
  const fetched = [];
  const errors = [];
  const sockets = [];
  let swRegistrations = 0;
  let reads = 0;
  let prompts = 0;
  // Timers are collected, never fired on their own: a case fires them itself
  // (fireTimers) to prove a timeout does what it claims.
  const timers = [];
  // A fetch that never settles, unless its abort signal fires — the shape of
  // a half-open connection.
  const hang = (signal) => new Promise((_, reject) => {
    if (signal) signal.onabort = () => reject(new Error("aborted"));
  });
  const ctx = {
    console: { log() {}, warn() {}, error: (...a) => { errors.push(a.map(String).join(" ")); }, info() {} },
    document: new Proxy({}, { get: () => el() }),
    localStorage: {
      getItem: (k) => (k in storage ? storage[k] : null),
      setItem: (k, v) => { storage[k] = String(v); },
      removeItem: (k) => { delete storage[k]; },
    },
    crypto: { randomUUID: () => "test-device-uuid" },
    location: { href: "http://x/", origin: "http://x", search: "", pathname: "/", protocol: "http:", host: "x" },
    navigator: { userAgent: "node", serviceWorker: {
      register: () => { swRegistrations++; return Promise.resolve({ pushManager: { getSubscription: async () => null } }); },
      addEventListener() {},
    } },
    WebSocket: function (url) { sockets.push(String(url)); return el(); },
    PushManager: function () {},
    // xterm.js, for the deep-link attach case: a terminal that accepts anything.
    Terminal: function () { return el(); },
    FitAddon: { FitAddon: function () { return el(); } },
    caches: { open: async () => ({
      match: async (k) => (pendingNav && k === "/__ccmux_pending_nav" ? { text: async () => pendingNav } : undefined),
      delete: async () => true,
    }) },
    setInterval: () => 0, setTimeout: (fn) => { timers.push(fn); return timers.length; }, clearInterval() {}, clearTimeout() {},
    addEventListener() {}, matchMedia: () => ({ matches: false, addEventListener() {} }),
    prompt: () => { prompts++; return "Patric"; }, alert() {}, confirm: () => true,
    AbortController: function () { const s = { onabort: null }; this.signal = s; this.abort = () => { if (s.onabort) s.onabort(); }; },
    Notification: { permission: "default" },
    URLSearchParams, URL, TextEncoder, TextDecoder,
    atob: (x) => x, btoa: (x) => x,
    fetch: async (url, opts) => {
      const u = String(url);
      fetched.push({ url: u, method: (opts && opts.method) || "GET", body: opts && opts.body });
      if (u.startsWith("/v1/workspaces")) return { ok: true, status: 200, json: async () => [] };
      if (u.startsWith("/v1/whoami")) {
        if (whoami === "hang") return hang(opts && opts.signal);
        if (whoami === null) return { ok: false, status: 404, json: async () => ({}) };
        if (whoamiGate) await whoamiGate;
        return { ok: true, status: 200, json: async () => whoami };
      }
      if (u.startsWith("/v1/windows?")) {
        const n = reads++;
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
    },
  };
  ctx.window = ctx; ctx.globalThis = ctx; ctx.self = ctx;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, "app.js"), "utf8"), ctx, { filename: "app.js" });
  if (withPush) vm.runInContext(fs.readFileSync(path.join(__dirname, "push.js"), "utf8"), ctx, { filename: "push.js" });
  // `state` is a top-level const, so it is a lexical binding and never lands on
  // the context object; only function declarations do. Later scripts in the same
  // context DO see it.
  const evalIn = (code) => vm.runInContext(code, ctx);
  const fireTimers = () => { const due = timers.splice(0); for (const fn of due) fn(); };
  return { ctx, fetched, errors, evalIn, prompts: () => prompts, fireTimers, sockets, swRegistrations: () => swRegistrations };
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
  check("a too-old daemon is not an error", oldLine.startsWith("Not identified"), oldLine);

  console.log(failures === 0 ? "web lens smoke: ok" : `web lens smoke: ${failures} failure(s)`);
  process.exit(failures === 0 ? 0 : 1);
})();
