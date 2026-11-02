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

// shell boots sw.js with a fake network and fake caches. `cached` is the build
// whose shell is already cached whole (every file's body "<build> <path>"),
// and netFor(path) says how the network answers: a build name to serve that
// build, "hang" for never, "down" for an error, or a promise of a build name
// to answer late. cacheBroken makes every cache
// read throw. Timers are held, so a case fires the 3s cutoff with expire().
function shell({ cached = "", netFor = () => "new", cacheBroken = false } = {}) {
  const handlers = {};
  const timers = [];
  const netCalls = [];
  const kept = [];
  const resp = (build, p) => ({
    ok: true, type: "basic", body: build + " " + p, clone() { return this; },
    headers: { get: (k) => (k.toLowerCase() === "etag" ? `"${build}"` : null) },
  });
  const pathOf = (r) => new URL(typeof r === "string" ? r : r.url, "http://x").pathname;
  const stores = new Map(); // cache name -> Map(path -> response)
  const store = (name) => { if (!stores.has(name)) stores.set(name, new Map()); return stores.get(name); };
  const src = fs.readFileSync(path.join(__dirname, "sw.js"), "utf8");
  const shellPaths = JSON.parse(/const SHELL = (\[[^\]]*\])/.exec(src)[1].replace(/,\s*\]/, "]"));
  if (cached) {
    for (const p of shellPaths) store("ccmux-shell-" + cached).set(p, resp(cached, p));
    store("ccmux-nav").set("/__ccmux_shell_build", { text: async () => cached });
  }
  const ctx = {
    self: { addEventListener: (type, fn) => { handlers[type] = fn; } },
    location: { origin: "http://x" },
    URL, Promise, console: { warn() {} }, JSON, Date,
    Response: function (body) { this.text = async () => String(body); },
    setTimeout: (fn) => timers.push(fn),
    caches: {
      open: async (name) => {
        if (cacheBroken) throw new Error("storage evicted");
        const m = store(name);
        return { match: async (r) => m.get(pathOf(r)), put: async (r, v) => { m.set(pathOf(r), v); } };
      },
      keys: async () => [...stores.keys()],
      delete: async (name) => stores.delete(name),
    },
    fetch: (r) => {
      const p = pathOf(r);
      netCalls.push(p);
      const how = netFor(p);
      if (how === "hang") return new Promise(() => {});
      if (how === "down") return Promise.reject(new Error("offline"));
      if (how instanceof Promise) return how.then((b) => resp(b, p)); // answers when the case says
      return Promise.resolve(resp(how, p));
    },
  };
  vm.createContext(ctx);
  vm.runInContext(src, ctx, { filename: "sw.js" });
  const settle = () => new Promise((r) => setImmediate(r));
  return {
    netCalls,
    // cachedBody says what the complete shell cache holds for a path.
    async cachedBody(p) {
      const b = await store("ccmux-nav").get("/__ccmux_shell_build")?.text();
      const r = b && store("ccmux-shell-" + b).get(p);
      return r ? r.body : "";
    },
    shellCaches: () => [...stores.keys()].filter((k) => k.startsWith("ccmux-shell-")),
    // get dispatches a fetch and returns the promise handed to respondWith,
    // failed if it has not settled within a second: a load that hangs is a
    // finding, and must not hang the gate with it.
    get(p, { navigate = false, clientId = "", resultingClientId = "" } = {}) {
      let answer = null;
      handlers.fetch({
        request: { url: "http://x" + p, method: "GET", mode: navigate ? "navigate" : "no-cors" },
        clientId, resultingClientId, respondWith: (a) => { answer = a; }, waitUntil: (w) => { kept.push(w); },
      });
      if (!answer) return null;
      let timer;
      const deadline = new Promise((_, reject) => {
        timer = global.setTimeout(() => reject(new Error(p + " never answered")), 1000);
      });
      return Promise.race([answer, deadline]).finally(() => global.clearTimeout(timer));
    },
    async expire() { await settle(); timers.splice(0).forEach((fn) => fn()); await settle(); },
    async background() { await settle(); await Promise.all(kept.splice(0)); await settle(); },
  };
}

test("a page the network is slow to send comes from the cache, and so does every file it loads", async () => {
  const sw = shell({ cached: "old", netFor: (p) => (p === "/" ? "hang" : "new") });
  const page = sw.get("/", { navigate: true, resultingClientId: "c1" });
  await sw.expire();
  assert.strictEqual((await page).body, "old /");
  const calls = sw.netCalls.length;
  const build = await sw.get("/build.js", { clientId: "c1" });
  assert.strictEqual(build.body, "old /build.js", "the cached page gets the cached build, never the new one");
  assert.strictEqual(sw.netCalls.length, calls, "nothing of a cached page goes to the network");
});

test("a page the network sends in time takes its files from the network, and the cache catches up whole", async () => {
  const sw = shell({ cached: "old" });
  assert.strictEqual((await sw.get("/", { navigate: true, resultingClientId: "c2" })).body, "new /");
  await sw.expire(); // the cutoff firing late changes nothing
  assert.strictEqual((await sw.get("/build.js", { clientId: "c2" })).body, "new /build.js");
  await sw.background();
  assert.strictEqual(await sw.cachedBody("/"), "new /");
  assert.strictEqual(await sw.cachedBody("/app.js"), "new /app.js");
  assert.strictEqual(JSON.stringify(sw.shellCaches()), JSON.stringify(["ccmux-shell-new"]), "the old build is dropped");
});

test("a page answer that arrives after the cache served it refreshes the cache whole, not that one file", async () => {
  let release;
  const late = new Promise((r) => { release = r; });
  const sw = shell({ cached: "old", netFor: (p) => (p === "/" ? late : "new") });
  const page = sw.get("/", { navigate: true, resultingClientId: "c3" });
  await sw.expire();
  assert.strictEqual((await page).body, "old /");
  assert.strictEqual(await sw.cachedBody("/"), "old /", "nothing changes before the network answers");
  release("new");
  await sw.background();
  assert.strictEqual(await sw.cachedBody("/"), "new /");
  assert.strictEqual(await sw.cachedBody("/build.js"), "new /build.js", "page and build.js come from one build");
});

test("a deploy landing mid-refresh leaves the cache on its old build, whole", async () => {
  const sw = shell({ cached: "old", netFor: (p) => (p === "/app.js" ? "newer" : "new") });
  await sw.get("/", { navigate: true, resultingClientId: "c4" });
  await sw.background();
  assert.strictEqual(await sw.cachedBody("/"), "old /");
  assert.strictEqual(await sw.cachedBody("/app.js"), "old /app.js");
  assert.strictEqual(await sw.cachedBody("/build.js"), "old /build.js");
});

test("with the daemon down the cached page opens; with nothing cached the load fails", async () => {
  const down = shell({ cached: "old", netFor: () => "down" });
  assert.strictEqual((await down.get("/", { navigate: true, resultingClientId: "c5" })).body, "old /");
  assert.strictEqual((await down.get("/app.js", { clientId: "c5" })).body, "old /app.js");
  const empty = shell({ netFor: () => "down" });
  await assert.rejects(empty.get("/", { navigate: true, resultingClientId: "c6" }), /offline/);
});

test("a cache that cannot be read fails the load instead of hanging it", async () => {
  const sw = shell({ cacheBroken: true, netFor: () => "down" });
  await assert.rejects(sw.get("/", { navigate: true, resultingClientId: "c7" }), /offline/);
});

test("a page from the network takes nothing from the cache, even when a file fails", async () => {
  const sw = shell({ cached: "old", netFor: (p) => (p === "/app.js" ? "down" : "new") });
  await assert.rejects(sw.get("/app.js", { clientId: "c8" }), /offline/);
  assert.strictEqual((await sw.get("/style.css", { clientId: "c8" })).body, "new /style.css");
});

test("the API is never the service worker's", () => {
  const sw = shell();
  assert.strictEqual(sw.get("/v1/health", { clientId: "c9" }), null);
  assert.strictEqual(sw.netCalls.length, 0);
});
