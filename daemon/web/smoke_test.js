// Runtime smoke test for the web lens. Run: node daemon/web/smoke_test.js
//
// WHY THIS EXISTS: daemon/web has no build step, bundler or lint, so the only
// gate was `node --check`, which is a parse. A parse cannot see a const declared
// inside a function and referenced from module scope — that shipped a lens whose
// sidebar was permanently empty on every load, because fetchWorkspaces caught its
// own ReferenceError and blanked the list. Measured: `node --check` passes on
// that exact mutation; this file fails on it.
//
// It is a SMOKE test, not a DOM: the stubs below are the minimum to get app.js
// evaluated and one code path driven. If app.js starts touching something new at
// load, add a stub rather than deleting the test — the whole value is that it
// evaluates the real file.
"use strict";
const fs = require("fs");
const path = require("path");
const vm = require("vm");

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

function run({ windowsOk }) {
  const fetched = [];
  const storage = {};
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
        return { ok: true, status: 200, json: async () => [
          { id: "win-here", name: "Here", workspaceIds: [], openBy: ["p"], open: true, openHere: true },
          { id: "win-elsewhere", name: "Elsewhere", workspaceIds: [], openBy: ["p"], open: true, openHere: false },
        ] };
      }
      return { ok: true, status: 200, text: async () => "", json: async () => ({}) };
    },
  };
  ctx.window = ctx; ctx.globalThis = ctx; ctx.self = ctx;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, "app.js"), "utf8"), ctx, { filename: "app.js" });
  return { ctx, fetched };
}

(async () => {
  // 1. The lens loads and its window path is reachable from module scope. This
  //    is the ReferenceError check: a mis-scoped const makes fetchWorkspaces
  //    throw into its own catch, and nothing below ever runs.
  const ok = run({ windowsOk: true });
  check("fetchWorkspaces is reachable", typeof ok.ctx.fetchWorkspaces === "function");
  if (typeof ok.ctx.fetchWorkspaces === "function") {
    await ok.ctx.fetchWorkspaces();
    await new Promise((r) => setImmediate(r));
    const win = ok.fetched.find((f) => f.url.startsWith("/v1/windows?"));
    check("window list asks for this device", win && win.url.includes("device=test-device-uuid"),
      win ? win.url : "no /v1/windows call at all");
    const set = ok.fetched.find((f) => f.url.includes("open-set"));
    check("declares its open set", !!set);
    if (set) {
      const body = JSON.parse(set.body);
      // Only openHere windows: the per-login `open` is true for both, and
      // declaring the other one would claim a window this lens does not have.
      check("declares only openHere windows", JSON.stringify(body.windowIds) === '["win-here"]',
        set.body);
      check("carries a device", body.device === "test-device-uuid", set.body);
      check("carries a readable label", /\(web\)$/.test(body.deviceLabel || ""), set.body);
    }
  }

  // 2. A failed window read declares NOTHING. open-set is authoritative, so an
  //    empty declaration means "I have none open" and the daemon clears this
  //    device's rows — for windows still on screen.
  const bad = run({ windowsOk: false });
  if (typeof bad.ctx.fetchWorkspaces === "function") {
    await bad.ctx.fetchWorkspaces();
    await new Promise((r) => setImmediate(r));
    check("no declaration on an unreadable window list",
      !bad.fetched.some((f) => f.url.includes("open-set")),
      "posted an empty set, which clears this device's rows");
  }

  console.log(failures === 0 ? "web lens smoke: ok" : `web lens smoke: ${failures} failure(s)`);
  process.exit(failures === 0 ? 0 : 1);
})();
