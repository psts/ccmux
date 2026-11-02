// sw.js's notification tap routing: which browser window a tap moves, what it
// stashes, and what a launch from a tap opens; and that it caches no lens file.
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

// The worker caches no lens file, and clears the shell caches earlier workers
// left behind. The notification state it keeps lives in ccmux-nav, which stays.
test("activating drops old shell caches and keeps the notification state", async () => {
  const names = new Set(["ccmux-shell-v32", "ccmux-shell-76017fff745c", "ccmux-nav", "other-app"]);
  const handlers = {};
  const ctx = {
    self: { addEventListener: (type, fn) => { handlers[type] = fn; }, skipWaiting() {}, clients: { claim: async () => {} } },
    URL, Promise, console,
    caches: { keys: async () => [...names], delete: async (k) => names.delete(k), open: async () => ({}) },
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, "sw.js"), "utf8"), ctx, { filename: "sw.js" });
  assert.strictEqual(handlers.fetch, undefined, "no fetch handler: every load goes to the daemon");
  let done;
  handlers.activate({ waitUntil: (p) => { done = p; } });
  await done;
  assert.deepStrictEqual([...names].sort(), ["ccmux-nav", "other-app"]);
});
