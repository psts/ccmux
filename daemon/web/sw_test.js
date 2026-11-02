// sw.js's notification tap routing: which browser window a tap moves, what it
// stashes, and what a launch from a tap opens; and its shell fetch: a page is
// all network or all cache, never a mix of two builds.
// Run: node --test daemon/web/sw_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const fs = require("fs");
const path = require("path");
const vm = require("vm");

// The key pages write the session -> window map under, read from pagewin.js
// itself: a rename on either side fails here instead of quietly routing every
// tap to the focused window.
const MAP_KEY = /const WS_WINDOWS = "([^"]+)"/.exec(fs.readFileSync(path.join(__dirname, "pagewin.js"), "utf8"))[1];

function client(url, focused) {
  const c = { url: "http://x" + url, focused, got: [], focusedNow: false };
  c.postMessage = (m) => c.got.push(m);
  c.focus = async () => { c.focusedNow = true; return c; };
  return c;
}

// tap boots sw.js against fake clients and a fake cache holding `map`, fires
// a notificationclick for url, and reports what happened.
async function tap(url, { clients = [], map = { "ws-1": "win-a", "ws-2": "win-b" } } = {}) {
  const store = new Map([[MAP_KEY, JSON.stringify(map)]]);
  const handlers = {};
  const opened = [];
  function Response(body) { this.body = String(body); }
  Response.prototype.text = async function () { return this.body; };
  Response.prototype.json = async function () { return JSON.parse(this.body); };
  const self = {
    location: { origin: "http://x" },
    addEventListener: (type, fn) => { handlers[type] = fn; },
    skipWaiting() {},
    clients: { matchAll: async () => clients, openWindow: async (u) => { opened.push(u); }, claim() {} },
    registration: { showNotification: async () => {} },
  };
  const ctx = {
    self, URL, Response, Date, JSON, Promise, console,
    caches: { open: async () => ({
      match: async (k) => (store.has(k) ? new Response(store.get(k)) : undefined),
      put: async (k, v) => { store.set(k, v.body); },
      delete: async (k) => store.delete(k),
    }), keys: async () => [], delete: async () => true },
    fetch: async () => { throw new Error("offline"); },
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, "sw.js"), "utf8"), ctx, { filename: "sw.js" });
  let done;
  handlers.notificationclick({ notification: { close() {}, data: { url } }, waitUntil: (p) => { done = p; } });
  await done;
  return { opened, stash: store.get("/__ccmux_pending_nav") || "" };
}

test("a tap moves only the browser window showing its session's window", async () => {
  const a = client("/?w=win-a&ws=ws-1", true);
  const b = client("/?w=win-b", false);
  const r = await tap("/?ws=ws-2", { clients: [a, b] });
  assert.strictEqual(a.got.length, 0, "the focused window that shows another window is left alone");
  // JSON: objects made inside the vm have its prototypes, not this realm's.
  assert.strictEqual(JSON.stringify(b.got), JSON.stringify([{ type: "ccmux-navigate", url: "/?ws=ws-2&w=win-b" }]));
  assert.ok(b.focusedNow);
  assert.match(r.stash, /^\/\?ws=ws-2&w=win-b&t=\d+$/, "the stash stays a plain URL an older page can read");
});

test("with no window showing it, the focused window takes the tap", async () => {
  const a = client("/?w=win-a", false);
  const p = client("/", true);
  await tap("/?ws=ws-2", { clients: [a, p] });
  assert.strictEqual(a.got.length, 0);
  assert.strictEqual(p.got.length, 1);
});

test("a tap with no window open launches one at the session's window", async () => {
  const r = await tap("/?ws=ws-2");
  assert.strictEqual(JSON.stringify(r.opened), JSON.stringify(["/?ws=ws-2&w=win-b"]));
});

test("a session the map does not know opens with no window pinned", async () => {
  const r = await tap("/?ws=loose");
  assert.strictEqual(JSON.stringify(r.opened), JSON.stringify(["/?ws=loose"]));
});

// shell boots sw.js with a fake network and a fake shell cache holding
// `cached` (path -> body). netFor(path) says how the network answers a path:
// "ok" at once, "hang" never, "down" with an error. Timers are held, so a
// case fires the 3s cutoff by hand with expire().
function shell({ cached = {}, netFor = () => "ok" } = {}) {
  const handlers = {};
  const timers = [];
  const netCalls = [];
  const resp = (body, from) => ({ ok: true, type: "basic", body, from, clone() { return this; } });
  const store = new Map(Object.entries(cached).map(([k, v]) => [k, resp(v, "cache")]));
  const pathOf = (r) => new URL(typeof r === "string" ? r : r.url, "http://x").pathname;
  const ctx = {
    self: { addEventListener: (type, fn) => { handlers[type] = fn; } },
    location: { origin: "http://x" },
    URL, Promise, console, JSON, Date,
    setTimeout: (fn) => timers.push(fn),
    caches: { open: async () => ({
      match: async (r) => store.get(pathOf(r)),
      put: async (r, v) => { store.set(pathOf(r), { ...v, from: "cache" }); },
    }) },
    fetch: (r) => {
      const p = pathOf(r);
      netCalls.push(p);
      const how = netFor(p);
      if (how === "hang") return new Promise(() => {});
      if (how === "down") return Promise.reject(new Error("offline"));
      return Promise.resolve(resp("net " + p, "net"));
    },
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, "sw.js"), "utf8"), ctx, { filename: "sw.js" });
  const settle = () => new Promise((r) => setImmediate(r));
  return {
    netCalls,
    // get dispatches a fetch and returns the promise handed to respondWith,
    // failed if it has not settled within a second: a load that hangs is a
    // finding, and must not hang the gate with it.
    get(p, { navigate = false, clientId = "", resultingClientId = "" } = {}) {
      let answer = null;
      handlers.fetch({
        request: { url: "http://x" + p, method: "GET", mode: navigate ? "navigate" : "no-cors" },
        clientId, resultingClientId, respondWith: (a) => { answer = a; },
      });
      if (!answer) return null;
      let timer;
      const deadline = new Promise((_, reject) => {
        timer = global.setTimeout(() => reject(new Error(p + " never answered")), 1000);
      });
      return Promise.race([answer, deadline]).finally(() => global.clearTimeout(timer));
    },
    async expire() { await settle(); timers.splice(0).forEach((fn) => fn()); await settle(); },
  };
}

test("a page the network is slow to send comes from the cache, and so does every file it loads", async () => {
  const sw = shell({ cached: { "/": "old page", "/build.js": "old build" }, netFor: (p) => (p === "/" ? "hang" : "ok") });
  const page = sw.get("/", { navigate: true, resultingClientId: "c1" });
  await sw.expire();
  assert.strictEqual((await page).body, "old page");
  const calls = sw.netCalls.length;
  const build = await sw.get("/build.js", { clientId: "c1" });
  assert.strictEqual(build.body, "old build", "the cached page gets the cached build, never the new one");
  assert.strictEqual(sw.netCalls.length, calls, "nothing of a cached page goes to the network");
});

test("a page the network sends in time takes its files from the network", async () => {
  const sw = shell({ cached: { "/": "old page", "/build.js": "old build" } });
  assert.strictEqual((await sw.get("/", { navigate: true, resultingClientId: "c2" })).from, "net");
  await sw.expire(); // the cutoff firing late changes nothing
  assert.strictEqual((await sw.get("/build.js", { clientId: "c2" })).body, "net /build.js");
});

test("with the daemon down the cached page opens; with nothing cached the load fails", async () => {
  const down = shell({ cached: { "/": "old page", "/app.js": "old app" }, netFor: () => "down" });
  assert.strictEqual((await down.get("/", { navigate: true, resultingClientId: "c3" })).body, "old page");
  assert.strictEqual((await down.get("/app.js", { clientId: "c3" })).body, "old app");
  const empty = shell({ netFor: () => "down" });
  await assert.rejects(empty.get("/", { navigate: true, resultingClientId: "c4" }));
});

test("a file of a network page falls back to the cache only when the network is down", async () => {
  const sw = shell({ cached: { "/app.js": "old app" }, netFor: (p) => (p === "/app.js" ? "down" : "ok") });
  assert.strictEqual((await sw.get("/app.js", { clientId: "c5" })).body, "old app");
  assert.strictEqual((await sw.get("/style.css", { clientId: "c5" })).from, "net");
});

test("the API is never the service worker's", () => {
  const sw = shell();
  assert.strictEqual(sw.get("/v1/health", { clientId: "c6" }), null);
  assert.strictEqual(sw.netCalls.length, 0);
});
