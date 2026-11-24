// node --test threads_test.js
//
// The thread page, booted in a small fake browser: every script threads.html
// loads, in its order, against a DOM that keeps a real tree (so a click
// finds the handler the page put there) and a daemon that records what it
// was asked. `node --check` only parses; this evaluates the page and drives
// what a human does on it: pick a repo, send the first message, answer a
// card, switch model, lose the connection.
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

// --- the fake DOM ---

function simple(sel) {
  const m = /^([a-zA-Z]+)?(#[\w-]+)?((?:\.[\w-]+)*)(\[([\w-]+)="([^"]*)"\])?$/.exec(sel);
  if (!m) throw new Error("selector not supported by the fake DOM: " + sel);
  return { tag: m[1], id: m[2] && m[2].slice(1), classes: m[3] ? m[3].split(".").filter(Boolean) : [], attr: m[5], value: m[6] };
}

class Node {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.className = "";
    this.dataset = {};
    this.style = {};
    this.attrs = {};
    this.value = "";
    this.text = "";
  }
  get classList() {
    const set = () => new Set(this.className.split(/\s+/).filter(Boolean));
    const put = (s) => { this.className = [...s].join(" "); };
    return {
      contains: (c) => set().has(c),
      add: (c) => put(set().add(c)),
      remove: (c) => { const s = set(); s.delete(c); put(s); },
      toggle: (c, on) => { const s = set(); const want = on === undefined ? !s.has(c) : !!on; if (want) s.add(c); else s.delete(c); put(s); return want; },
    };
  }
  get textContent() { return this.tagName === "#TEXT" ? this.text : this.children.map((c) => c.textContent).join(""); }
  set textContent(v) { this.children.forEach((c) => { c.parentNode = null; }); this.children = v === "" || v == null ? [] : [Object.assign(new Node("#text"), { text: String(v), parentNode: this })]; }
  // Markup set on a node keeps what page code looks up afterwards: every
  // element in it that has an id, hung flat under the node (see page()).
  set innerHTML(html) { this.textContent = ""; this.insertAdjacentHTML("beforeend", html); }
  insertAdjacentHTML(_where, html) { for (const el of this.ownerDocument.withIds(String(html))) this.appendChild(el); }
  get firstChild() { return this.children[0] || null; }
  get nextSibling() { const sibs = this.parentNode ? this.parentNode.children : []; return sibs[sibs.indexOf(this) + 1] || null; }
  get childElementCount() { return this.children.filter((c) => c.tagName !== "#TEXT").length; }
  get isConnected() { return true; }
  contains(n) { for (let p = n; p; p = p.parentNode) if (p === this) return true; return false; }
  remove() { if (this.parentNode) this.parentNode.children = this.parentNode.children.filter((c) => c !== this); this.parentNode = null; }
  insertBefore(node, before) {
    node.remove();
    node.parentNode = this;
    const i = before ? this.children.indexOf(before) : -1;
    if (i < 0) this.children.push(node); else this.children.splice(i, 0, node);
    return node;
  }
  appendChild(node) { return this.insertBefore(node, null); }
  append(...nodes) { nodes.forEach((n) => this.appendChild(n)); }
  prepend(node) { this.insertBefore(node, this.children[0] || null); }
  replaceWith(node) { const p = this.parentNode; p.insertBefore(node, this); this.remove(); }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  addEventListener(type, fn) { this["on" + type] = fn; }
  focus() { this.ownerDocument.activeElement = this; }
  click() { if (!this.disabled && this.onclick) return this.onclick({ target: this, preventDefault() {} }); }
  matches(sel) {
    const s = simple(sel);
    if (s.tag && s.tag.toUpperCase() !== this.tagName) return false;
    if (s.id && this.id !== s.id) return false;
    if (s.attr && this.dataset[s.attr.replace(/^data-/, "")] !== s.value) return false;
    return s.classes.every((c) => this.classList.contains(c));
  }
  all() { return this.children.flatMap((c) => (c.tagName === "#TEXT" ? [] : [c, ...c.all()])); }
  querySelectorAll(sel) {
    const parts = sel.trim().split(/\s+/);
    const last = parts.pop();
    const under = (node, rest) => {
      if (!rest.length) return true;
      for (let p = node.parentNode; p; p = p.parentNode) if (p.matches && p.tagName !== "#TEXT" && p.matches(rest[rest.length - 1])) return under(p, rest.slice(0, -1));
      return false;
    };
    return this.all().filter((n) => n.matches(last) && under(n, parts));
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
}

// page builds the document threads.html describes: every element that has an
// id, hung flat under the body with its tag and classes.
// Markup a script adds later (the Settings window makes its own) goes through
// the same reading: its elements with an id, findable by that id.
function page() {
  const html = fs.readFileSync(path.join(__dirname, "threads.html"), "utf8");
  const body = new Node("body");
  const byId = new Map();
  const doc = {
    body, activeElement: null, visibilityState: "visible", title: "", handlers: {},
    createElement: (tag) => Object.assign(new Node(tag), { ownerDocument: doc }),
    createTextNode: (text) => Object.assign(new Node("#text"), { text: String(text) }),
    getElementById: (id) => byId.get(id) || null,
    querySelector: (sel) => body.querySelector(sel),
    querySelectorAll: (sel) => body.querySelectorAll(sel),
    addEventListener: (type, fn) => { doc.handlers[type] = fn; },
    dispatchEvent: (ev) => { if (doc.handlers[ev.type]) doc.handlers[ev.type](ev); },
    withIds: (markup) => [...markup.matchAll(/<(\w+)([^>]*\bid="([^"]+)"[^>]*)>/g)].map((m) => {
      const el = doc.createElement(m[1]);
      el.id = m[3];
      el.className = (/\bclass="([^"]*)"/.exec(m[2]) || [, ""])[1];
      byId.set(el.id, el);
      return el;
    }),
  };
  body.ownerDocument = doc;
  body.insertAdjacentHTML("beforeend", html);
  byId.get("composer").appendChild(doc.createElement("label"));
  return doc;
}

// --- the fake daemon and browser ---

const scripts = [...fs.readFileSync(path.join(__dirname, "threads.html"), "utf8").matchAll(/<script src="\/([^"]+)"/g)].map((m) => m[1]).filter((f) => f !== "build.js" && f !== "update.js");

const repo = { id: "ws1", name: "app", repoPath: "/p/app", git: { isGitRepo: true, branch: "dev" } };
const lead = { id: "lead1", workspaceId: "ws1", kind: "lead", cwd: "/p/app", model: "", state: "running", attention: "idle", paneId: "pane1", createdAt: 1 };

async function boot(opts = {}) {
  const doc = page();
  const book = { calls: [], sockets: [], timers: [], intervals: [], frames: [], pushed: [], opened: [] };
  const workspaces = opts.workspaces || [repo];
  const lists = { windows: opts.windows || [{ id: "w1", name: "ChartLabs", workspaceIds: workspaces.map((w) => w.id) }], workspaces, sessions: opts.sessions || [] };
  const routes = [
    ...(opts.routes || []), // a case's own answers win over the usual ones
    [/^GET \/v1\/windows/, () => lists.windows], [/^GET \/v1\/workspaces/, () => lists.workspaces], [/^GET \/v1\/sessions$/, () => ({ sessions: lists.sessions })],
    [/^GET \/v1\/whoami/, () => ({ vouched: true, login: "pat@example.invalid", display: "Pat" })],
    [/^POST \/v1\/sessions$/, (body) => { const made = { ...lead, id: body.kind === "lead" ? "lead1" : "t1", kind: body.kind || "", model: body.model }; lists.sessions = [...lists.sessions, made]; return made; }],
    [/^POST \/v1\/sessions\/[^/]+\/images$/, () => ({ name: "0123456789abcdef0123456789abcdef.png" })],
  ];
  const fetch = async (url, init = {}) => {
    const method = init.method || "GET";
    if (opts.listsWait && /^\/v1\/(windows|workspaces|sessions)$/.test(url) && method === "GET") await opts.listsWait;
    const body = typeof init.body === "string" ? JSON.parse(init.body) : init.body;
    book.calls.push({ method, url: String(url), body });
    const hit = routes.find(([re]) => re.test(method + " " + url));
    const out = hit ? await hit[1](body) : {};
    if (out && out.fail) return { ok: false, status: out.fail, json: async () => ({ error: out.error }) };
    return { ok: true, status: 200, json: async () => out };
  };
  const ctx = {
    console, document: doc, fetch, URLSearchParams, URL: Object.assign(function (...a) { return new URL(...a); }, { createObjectURL: () => "blob:x", revokeObjectURL() {} }), Blob,
    location: { search: opts.search || "", protocol: "http:", host: "d", href: "" },
    history: { pushState: (_s, _t, url) => book.pushed.push(url) },
    localStorage: opts.storage || { getItem: () => "stored-name" }, navigator: opts.push ? pushBrowser(book).navigator : {},
    WebSocket: function (url) { const s = { url: String(url), readyState: 1, sent: [], send: (x) => s.sent.push(JSON.parse(x)), close() { s.closed = true; } }; book.sockets.push(s); return s; },
    requestAnimationFrame: (fn) => book.frames.push(fn), setTimeout: (fn, ms) => { book.timers.push({ fn, ms }); return book.timers.length; }, clearTimeout() {}, setInterval: (fn, ms) => { book.intervals.push({ fn, ms }); },
    open: (url, target) => book.opened.push([url, target]),
    addEventListener() {}, confirm: opts.confirm || (() => true), prompt: opts.prompt || (() => "Renamed"), createImageBitmap: async () => ({ width: 10, height: 10 }),
    // The Settings window's tabs: identity.js bounds its whoami read, and the
    // Agents tab says on document when it read the bases.
    AbortController, Event: function (type) { this.type = type; },
    ...(opts.push ? pushBrowser(book).globals : {}),
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  for (const f of scripts) vm.runInContext(fs.readFileSync(path.join(__dirname, f), "utf8"), ctx, { filename: f });
  const settle = async () => { for (let i = 0; i < 12; i++) await new Promise((r) => setImmediate(r)); book.frames.splice(0).forEach((fn) => fn()); };
  await settle();
  const $ = (id) => doc.getElementById(id);
  const stream = () => book.sockets.filter((s) => s.url.includes("/ws?")).pop();
  const frame = async (f) => { stream().onmessage({ data: JSON.stringify(f) }); await settle(); };
  return { doc, book, $, settle, stream, frame, lists, posts: () => book.calls.filter((c) => c.method === "POST"), where: () => ctx.location.href };
}

// pushBrowser is a browser that can do web push, for the Notifications tab:
// a service worker that registers, a push manager that holds one
// subscription, and a cache whose every open is counted (the stash a
// notification tap waits in). Made once per boot and kept on the book.
function pushBrowser(book) {
  if (book.push) return book.push;
  Object.assign(book, { swMessage: [], cacheOpens: 0, subscription: null });
  const made = { endpoint: "https://push.example.invalid/abc", toJSON: () => ({ endpoint: made.endpoint, keys: { p256dh: "k", auth: "a" } }), unsubscribe: async () => { book.subscription = null; } };
  const reg = { pushManager: { getSubscription: async () => book.subscription, subscribe: async () => { book.subscription = made; return made; } } };
  book.push = {
    navigator: { userAgent: "node", serviceWorker: { register: async () => reg, ready: Promise.resolve(reg), addEventListener: (type, fn) => { if (type === "message") book.swMessage.push(fn); } } },
    globals: {
      PushManager: function () {}, Notification: { permission: "default", requestPermission: async () => "granted" },
      atob: (x) => Buffer.from(x, "base64").toString("binary"), Uint8Array,
      caches: { open: async () => { book.cacheOpens++; return { match: async () => undefined, delete: async () => true }; } },
    },
  };
  return book.push;
}

const snapshot = (over = {}) => ({ t: "snapshot", boot: "b1", seq: 5, data: { messages: [{ info: { id: "u1", role: "user" }, parts: [{ id: "u1.0", messageID: "u1", type: "text", text: "hello there" }] }], permissions: [], questions: [], info: {}, todos: [], ...over } });
const question = { id: "qqqqq", questions: [{ question: "Red or blue?", header: "Colour", options: [{ label: "Red", description: "warm" }, { label: "Blue", description: "cool" }], multiple: false, custom: true }] };

test("the page boots: the sidebar from the daemon's lists, the firehose under the vouched name", async () => {
  const p = await boot({ sessions: [lead] });
  assert.deepEqual(p.$("tree").querySelectorAll(".group-name").map((n) => n.textContent), ["ChartLabs"]);
  assert.deepEqual(p.$("tree").querySelectorAll(".row-name").map((n) => n.textContent), ["app"]);
  assert.equal(p.$("tree").querySelector(".branch").textContent, "dev");
  assert.equal(p.book.sockets[0].url, "ws://d/v1/events?user=Pat&device=web");
  p.book.sockets[0].onopen();
  assert.deepEqual(p.book.sockets[0].sent, [{ t: "focus", present: true }], "a person at this page counts as at a screen");
  assert.equal(p.$("composer").classList.contains("hidden"), true, "nothing picked, nothing to write to");
});

test("a repo with no lead opens as a draft, and its first message makes the lead and then sends", async () => {
  const p = await boot();
  p.$("tree").querySelector(".row").click();
  await p.settle();
  assert.equal(p.book.pushed.pop(), "/threads.html?ws=ws1&new=lead");
  assert.equal(p.posts().length, 0, "looking at a repo starts nothing");
  assert.equal(p.$("title").textContent, "app");
  assert.equal(p.$("new-thread").classList.contains("hidden"), false, "a repo's header offers New thread");
  p.$("input").value = "What landed since yesterday?";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.deepEqual(p.posts().map((c) => [c.url, c.body]), [
    ["/v1/sessions", { cwd: "/p/app", workspaceId: "ws1", model: "", advisor: "", kind: "lead" }],
    ["/v1/sessions/lead1/prompt", { text: "What landed since yesterday?", sender: "Pat", images: [] }],
  ]);
  assert.equal(p.book.pushed.pop(), "/threads.html?s=lead1");
  assert.match(p.stream().url, /\/v1\/sessions\/lead1\/ws\?boot=&after=0$/);
  assert.equal(p.$("input").value, "", "sent, so the composer is empty");
});

test("a draft offers the commands its repo's last session said, before its own session starts", async () => {
  const kept = { "ccmux-user": "stored-name" };
  const storage = { getItem: (k) => kept[k] || null, setItem: (k, v) => { kept[k] = v; } };
  const seen = await boot({ sessions: [lead], search: "?s=lead1", storage });
  await seen.frame(snapshot({ info: { commands: [{ name: "review", description: "Review the diff", hint: "" }, { name: "doctor", description: "", hint: "" }], terminalCommands: ["doctor"] } }));
  const p = await boot({ sessions: [lead], search: "?ws=ws1&new=thread", storage });
  p.$("input").value = "/";
  p.$("input").oninput();
  assert.deepEqual(p.$("commands").querySelectorAll(".cmd-name").map((n) => n.textContent), ["/review"], "terminal-only commands stay hidden");
  assert.equal(p.posts().length, 0, "showing the list starts nothing");
  const cold = await boot({ search: "?ws=ws1&new=thread" });
  cold.$("input").value = "/";
  cold.$("input").oninput();
  assert.equal(cold.$("commands").classList.contains("hidden"), true, "no list: no menu over the line being typed");
  p.$("input").value = "/nosuch";
  p.$("input").oninput();
  assert.equal(p.$("commands").classList.contains("hidden"), true, "nothing matches: no menu either");
});

test("a session's commands are remembered even when its snapshot lands before the lists", async () => {
  const kept = {};
  const storage = { getItem: (k) => kept[k] || null, setItem: (k, v) => { kept[k] = v; } };
  let lists;
  const p = await boot({ sessions: [lead], search: "?s=lead1", storage, listsWait: new Promise((r) => { lists = r; }) });
  await p.frame(snapshot({ info: { commands: [{ name: "review", description: "", hint: "" }] } }));
  assert.equal(kept["ccmux-thread-commands"], undefined, "whose session it is is not known yet");
  lists();
  await p.settle();
  assert.deepEqual(Object.keys(JSON.parse(kept["ccmux-thread-commands"])), ["ws1:lead"]);
});

test("a message that cannot be sent stays where it was sent, says why, and is sent again by Retry", async () => {
  let fail = true;
  const p = await boot({ sessions: [lead], search: "?s=lead1", routes: [[/^POST \/v1\/sessions\/lead1\/prompt/, () => (fail ? { fail: 409, error: "this session has ended; start a new one" } : { id: "p1" })]] });
  p.$("input").value = "still there?";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.equal(p.$("input").value, "", "the text stays in the conversation, not the composer");
  const line = p.$("blocks").querySelector(".sending");
  assert.equal(line.className, "sending failed");
  assert.match(line.textContent, /^Not sent: this session has ended; start a new one/);
  fail = false;
  line.querySelectorAll("button").find((b) => b.textContent === "Retry").click();
  await p.settle();
  assert.deepEqual(p.posts().map((c) => c.body.text), ["still there?", "still there?"]);
  assert.equal(p.$("blocks").querySelector(".sending").textContent, "Sent, waiting for Claude");
});

test("a failed message holds the ones after it until it is removed, and stays in its own conversation", async () => {
  const other = { ...lead, id: "t2", kind: "", title: "Other", createdAt: 2 };
  let n = 0;
  const p = await boot({ sessions: [lead, other], search: "?s=lead1", routes: [[/^POST \/v1\/sessions\/lead1\/prompt/, () => (++n === 1 ? { fail: 500, error: "boom" } : { id: "p" + n })]] });
  p.$("input").value = "first";
  p.$("composer").onsubmit({ preventDefault() {} });
  p.$("input").value = "second";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.deepEqual(p.posts().map((c) => c.body.text), ["first"], "second does not go ahead of first");
  assert.deepEqual(p.$("blocks").querySelectorAll(".sending").map((x) => x.textContent.split(" Retry")[0]), ["Not sent: boom", "Waits for the message above"]);
  p.$("tree").querySelectorAll(".row").find((r) => r.textContent.includes("Other")).click();
  await p.settle();
  assert.equal(p.$("input").value, "", "nothing of lead1's lands in another conversation's composer");
  assert.equal(p.$("blocks").querySelectorAll(".b-user").length, 0);
  p.$("tree").querySelectorAll(".row").find((r) => r.textContent.includes("app")).click();
  await p.settle();
  p.$("blocks").querySelector(".sending").querySelectorAll("button").find((b) => b.textContent === "Remove").click();
  await p.settle();
  assert.deepEqual(p.posts().map((c) => c.body.text), ["first", "second"], "removing first lets second go");
  assert.deepEqual(p.$("blocks").querySelectorAll(".bubble").map((x) => x.textContent), ["second"]);
});

test("a message shows the moment it is sent, and gives way to its turn once the stream has it", async () => {
  let answer;
  const held = new Promise((r) => { answer = r; });
  const p = await boot({ sessions: [lead], search: "?s=lead1", routes: [[/^POST \/v1\/sessions\/lead1\/prompt/, () => held]] });
  await p.frame(snapshot());
  p.$("input").value = "quick one";
  p.$("composer").onsubmit({ preventDefault() {} });
  assert.equal(p.$("input").value, "", "the composer is free at once");
  const bubbles = () => p.$("blocks").querySelectorAll(".bubble").map((b) => b.textContent);
  assert.deepEqual(bubbles(), ["hello there", "quick one"], "drawn before the daemon answered");
  assert.equal(p.$("blocks").querySelector(".sending").textContent, "Sending…");
  answer({ id: "p1" });
  await p.settle();
  assert.equal(p.$("blocks").querySelector(".sending").textContent, "Sent, waiting for Claude");
  await p.frame({ t: "event", seq: 6, data: { type: "message.updated", properties: { info: { id: "p1", role: "user" } } } });
  await p.frame({ t: "event", seq: 7, data: { type: "message.part.updated", properties: { part: { id: "p1.0", messageID: "p1", type: "text", text: "quick one" } } } });
  assert.deepEqual(bubbles(), ["hello there", "quick one"], "once, as the stream's own turn");
  assert.equal(p.$("blocks").querySelector(".sending"), null);
});

test("two messages sent quickly go out in order, after the first one's session is made", async () => {
  const p = await boot();
  p.$("tree").querySelector(".row").click();
  await p.settle();
  p.$("input").value = "first";
  p.$("composer").onsubmit({ preventDefault() {} });
  p.$("input").value = "second";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.deepEqual(p.posts().map((c) => [c.url, c.body && c.body.text]), [
    ["/v1/sessions", undefined], ["/v1/sessions/lead1/prompt", "first"], ["/v1/sessions/lead1/prompt", "second"],
  ]);
  assert.deepEqual(p.$("blocks").querySelectorAll(".bubble").map((b) => b.textContent), ["first", "second"], "both still on screen in the session the draft became");
});

test("the conversation draws from a snapshot and its events, and a hole in the numbers redials for a new snapshot", async () => {
  const p = await boot({ sessions: [lead], search: "?s=lead1" });
  await p.frame(snapshot());
  await p.frame({ t: "event", seq: 6, data: { type: "message.updated", properties: { info: { id: "m1", role: "assistant" } } } });
  await p.frame({ t: "event", seq: 7, data: { type: "message.part.updated", properties: { part: { id: "m1.0", messageID: "m1", type: "text", text: "Hi **Pat**" } } } });
  assert.deepEqual(p.$("blocks").children.map((b) => b.className), ["b-user", "b-claude"]);
  assert.equal(p.$("blocks").querySelector(".bubble").textContent, "hello there");
  assert.equal(p.$("blocks").querySelector("strong").textContent, "Pat");
  const first = p.stream();
  await p.frame({ t: "event", seq: 9, data: { type: "message.part.updated", properties: { part: { id: "m1.1", messageID: "m1", type: "text", text: "after a hole" } } } });
  assert.equal(first.closed, true);
  assert.match(p.stream().url, /ws\?boot=&after=0$/, "no place is claimed, so the daemon sends the conversation whole");
  assert.equal(p.$("blocks").textContent.includes("after a hole"), false, "nothing past the hole was drawn");
});

test("a conversation that arrives before the lists is drawn again once they say whose it is", async () => {
  let release;
  const p = await boot({ sessions: [lead], search: "?s=lead1", listsWait: new Promise((r) => { release = r; }) });
  await p.frame(snapshot({ messages: [
    { info: { id: "u1", role: "user" }, parts: [{ id: "u1.0", messageID: "u1", type: "text", text: "look" }, { id: "u1.1", messageID: "u1", type: "file", mime: "image/png", name: "0123456789abcdef0123456789abcdef.png" }] },
    { info: { id: "m1", role: "assistant" }, parts: [{ id: "m1.0", messageID: "m1", type: "text", text: "seen" }] },
  ] }));
  assert.equal(p.$("blocks").querySelector("img").src, "/v1/sessions/lead1/media/0123456789abcdef0123456789abcdef.png", "the image is this session's, known from the address");
  assert.equal(p.$("blocks").querySelector(".who").textContent, "Claude");
  assert.equal(p.$("title").textContent, "…", "not called missing before the lists were read");
  release();
  await p.settle();
  assert.equal(p.$("title").textContent, "app");
  assert.equal(p.$("blocks").querySelector(".who").textContent, "Claude · app lead");
  assert.equal(p.$("blocks").children.length, 2, "drawn again, not twice");
});

test("a dropped connection dials again from where the page stands", async () => {
  const p = await boot({ sessions: [lead], search: "?s=lead1" });
  await p.frame(snapshot());
  await p.frame({ t: "event", seq: 6, data: { type: "session.idle", properties: {} } });
  const dropped = p.stream();
  dropped.onclose();
  assert.equal(p.book.timers.pop().ms >= 1000, true, "after a wait, not at once");
  p.book.timers.length = 0;
  dropped.onclose(); // a second close of the same socket schedules nothing
  assert.equal(p.book.timers.length, 0);
});

test("the wait over, the page asks for the events after the last one it has", async () => {
  const p = await boot({ sessions: [lead], search: "?s=lead1" });
  await p.frame(snapshot());
  await p.frame({ t: "event", seq: 6, data: { type: "session.idle", properties: {} } });
  p.stream().onclose();
  p.book.timers.pop().fn();
  assert.match(p.stream().url, /ws\?boot=b1&after=6$/);
});

test("each conversation keeps its own unsent text: it leaves with the conversation and comes back with it", async () => {
  const other = { ...lead, id: "t2", kind: "", title: "Other", createdAt: 2 };
  const p = await boot({ sessions: [lead, other], search: "?s=lead1" });
  const go = async (name) => { p.$("tree").querySelectorAll(".row").find((r) => r.textContent.includes(name)).click(); await p.settle(); };
  p.$("input").value = "half a thought for app";
  await go("Other");
  assert.equal(p.$("input").value, "", "another conversation starts with its own empty box");
  p.$("input").value = "for Other";
  await go("app");
  assert.equal(p.$("input").value, "half a thought for app", "the text comes back with its conversation");
  await go("Other");
  assert.equal(p.$("input").value, "for Other");
});

test("a picture attached in one conversation stays there and is not sent from another", async () => {
  const other = { ...lead, id: "t2", kind: "", title: "Other", createdAt: 2 };
  const p = await boot({ sessions: [lead, other], search: "?s=lead1", routes: [[/^POST \/v1\/sessions\/t2\/prompt/, () => ({ id: "p1" })]] });
  const go = async (name) => { p.$("tree").querySelectorAll(".row").find((r) => r.textContent.includes(name)).click(); await p.settle(); };
  p.$("file").onchange({ target: { files: [{ type: "image/png", size: 100, name: "shot.png" }], value: "x" } });
  assert.equal(p.$("attached").children.length, 1);
  await go("Other");
  assert.equal(p.$("attached").children.length, 0, "Other opens with nothing attached");
  p.$("input").value = "words only";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.deepEqual(p.posts().map((c) => [c.url, c.body.images]), [["/v1/sessions/t2/prompt", []]], "lead1's picture is not sent to Other");
  await go("app");
  assert.equal(p.$("attached").children.length, 1, "the picture waited in its own conversation");
});

test("a question card stays with its conversation: another one opens without it", async () => {
  const other = { ...lead, id: "t2", kind: "", title: "Other", createdAt: 2 };
  const p = await boot({ sessions: [lead, other], search: "?s=lead1" });
  await p.frame(snapshot({ questions: [question] }));
  assert.equal(p.$("cards").children.length, 1);
  p.$("tree").querySelectorAll(".row").find((r) => r.textContent.includes("Other")).click();
  await p.settle();
  assert.equal(p.$("cards").children.length, 0, "lead1's question is not on Other's screen");
});

test("a question is answered by a tap or by its key, through the session's card route", async () => {
  const p = await boot({ sessions: [lead], search: "?s=lead1" });
  await p.frame(snapshot({ questions: [question] }));
  const card = p.$("cards").querySelector(".ask");
  assert.deepEqual(card.querySelectorAll(".key").map((k) => k.textContent), ["1", "2"]);
  // A digit typed into the composer is a digit.
  p.doc.handlers.keydown({ key: "2", target: p.$("input"), preventDefault() {} });
  await p.settle();
  assert.equal(p.posts().length, 0);
  p.doc.handlers.keydown({ key: "2", target: p.doc.body, preventDefault() {} });
  await p.settle();
  assert.deepEqual(p.posts().map((c) => [c.url, c.body]), [["/v1/sessions/lead1/cards/qqqqq", { kind: "question", answers: [["Blue"]] }]]);
  p.doc.handlers.keydown({ key: "4", target: p.doc.body, preventDefault() {} });
  await p.settle();
  assert.equal(p.posts().length, 1, "a key with no choice behind it answers nothing");
});

test("a permission is allowed or refused by key, and an answer the daemon will not take is said on the card", async () => {
  const perm = { id: "abcde", permission: "Write", patterns: ["/p/app/a.txt"], always: ["*"], tool: { callID: "c1" } };
  const p = await boot({ sessions: [lead], search: "?s=lead1", routes: [[/^POST \/v1\/sessions\/lead1\/cards\/abcde/, () => ({ fail: 409, error: "that card is no longer waiting" })]] });
  await p.frame(snapshot({ permissions: [perm] }));
  const buttons = p.$("cards").querySelectorAll(".ac-perm-btn");
  assert.deepEqual(buttons.map((b) => b.dataset.key), ["1", "2", "3"]);
  p.doc.handlers.keydown({ key: "3", target: p.doc.body, preventDefault() {} });
  await p.settle();
  assert.deepEqual(p.posts().map((c) => c.body), [{ kind: "permission", reply: "reject" }]);
  const err = p.$("cards").querySelector(".ask-error");
  assert.equal(err.textContent, "Not sent: that card is no longer waiting");
  assert.equal(buttons[0].disabled, false, "the card can be answered again");
  await p.frame({ t: "event", seq: 6, data: { type: "permission.replied", properties: { requestID: "abcde" } } });
  assert.equal(p.$("cards").children.length, 0);
});

test("a model picked is saved, said to apply on the next restart, and Restart now waits for the new runner", async () => {
  const p = await boot({ sessions: [{ ...lead, model: "opus" }], search: "?s=lead1" });
  await p.frame(snapshot({ info: { model: "claude-opus-5-5" } }));
  assert.equal(p.$("model-bar").classList.contains("hidden"), true);
  p.$("model").onchange({ target: { value: "haiku" } });
  await p.settle();
  assert.deepEqual(p.posts().map((c) => [c.url, c.body]), [["/v1/sessions/lead1/model", { model: "haiku" }]]);
  assert.equal(p.$("model-bar").classList.contains("hidden"), false);
  assert.equal(p.$("model-bar-text").textContent, "Haiku applies on the next restart. This session keeps running on Opus until then.");
  p.$("later").click();
  await p.settle();
  assert.equal(p.$("model-bar").classList.contains("hidden"), true);
  assert.equal(p.$("restart-later").textContent, "Restart to apply Haiku");
  assert.equal(p.$("restart-later").classList.contains("hidden"), false);
  p.$("restart-later").click();
  await p.settle();
  assert.equal(p.posts().pop().url, "/v1/sessions/lead1/restart");
  assert.equal(p.$("restarted-bar").classList.contains("hidden"), true, "not restarted until the new runner says so");
  await p.frame({ t: "snapshot", boot: "b2", seq: 0, data: { messages: [], info: { model: "claude-haiku-5-5" } } });
  assert.equal(p.$("restarted-bar").classList.contains("hidden"), false);
  assert.equal(p.$("restarted-bar").textContent, "Restarted on Haiku. The session picked up where it was.");
});

test("an advisor picked is saved and applies on the next restart, on the same bar as a model", async () => {
  const p = await boot({ sessions: [{ ...lead, model: "opus" }], search: "?s=lead1" });
  await p.frame(snapshot({ info: { model: "claude-opus-5-5" } }));
  assert.equal(p.$("advisor-box").classList.contains("hidden"), false, "a runner from before the picker shows it too");
  assert.equal(p.$("advisor").children[0].textContent, "Settings", "it cannot say what the settings name");
  await p.frame(snapshot({ info: { model: "claude-opus-5-5", advisor: { model: "fable", from: "settings", settings: "fable" } } }));
  assert.equal(p.$("advisor-box").classList.contains("hidden"), false);
  assert.deepEqual([...p.$("advisor").children].map((o) => o.value), ["", "fable", "opus"]);
  p.$("advisor").onchange({ target: { value: "opus" } });
  await p.settle();
  assert.deepEqual(p.posts().map((c) => [c.url, c.body]), [["/v1/sessions/lead1/advisor", { advisor: "opus" }]]);
  assert.equal(p.$("model-bar").classList.contains("hidden"), false);
  assert.equal(p.$("model-bar-text").textContent, "Advisor Opus applies on the next restart. This session keeps Fable as its advisor until then.");
  p.$("restart-now").click();
  await p.settle();
  assert.equal(p.posts().pop().url, "/v1/sessions/lead1/restart");
});

test("a repo's git details open on a click on the repo the page is in, never on the way to another, stay open beside others, and close on the next click", async () => {
  const other = { id: "ws2", name: "web", repoPath: "/p/web", git: { isGitRepo: true, branch: "main" } };
  const app = { ...repo, git: { isGitRepo: true, branch: "dev", trackingBranch: "origin/dev", ahead: 2, defaultBranch: "main", defaultRef: "origin/main", aheadOfDefault: 74, modifiedFiles: ["a.ts"] } };
  const kept = {};
  const storage = { getItem: (k) => kept[k] || null, setItem: (k, v) => { kept[k] = v; } };
  const p = await boot({ workspaces: [app, other], sessions: [lead], search: "?s=lead1", storage });
  const rows = () => p.$("tree").querySelectorAll(".row");
  const opened = () => rows().map((r) => !!r.querySelector(".git-detail"));
  assert.deepEqual(opened(), [false, false], "nothing opened yet");
  assert.deepEqual(rows()[0].querySelector(".row-top").children.map((c) => c.className + ":" + c.textContent), ["row-name:app", "row-status:●1↑2↑74", "branch:dev"], "name, then changes, ahead and what is not shipped, then the branch last");
  const ship = rows()[0].querySelector(".ship");
  assert.equal(ship.title, "74 commits not on origin/main yet: ready to ship");
  assert.deepEqual(rows()[1].querySelector(".row-top").children.map((c) => c.className + ":" + c.textContent), ["row-name:web", "row-status:", "branch:main"], "a clean repo keeps its empty column, so the branches line up");
  rows()[0].click();
  await p.settle();
  assert.equal(p.book.pushed.length, 0, "the repo the page is in: no move");
  assert.equal(rows()[0].className, "row on open");
  assert.deepEqual(rows()[0].querySelector(".git-detail").children.map((c) => c.textContent), ["dev → origin/dev ↑2", "vs origin/main ↑74", "● 1 changed file"], "no folder: it is in the row's hover tip");
  assert.deepEqual(p.$("tree").querySelectorAll(".repo")[0].children.map((c) => c.className), ["row on open", "side-more", "side-todo"], "beside the card, not inside it: its menu button, which only a phone shows (threads.css), and the repo's to-do pill, both laid over the card");
  const pill = p.$("tree").querySelectorAll(".repo")[0].querySelector(".side-todo");
  assert.equal(pill.querySelector(".sd-count").className, "sd-count", "the repo the page is in shows its pill with nothing on its list, so a first item can be added");
  assert.equal(pill.textContent, "0");
  assert.equal(pill.title, "To-do: nothing open");
  assert.ok(p.$("tree").querySelectorAll(".repo")[0].classList.contains("has-todo"), "its row keeps a column free for the pill");
  assert.equal(p.$("tree").querySelectorAll(".repo")[1].querySelector(".side-todo"), null, "another repo with nothing on its list has none");
  rows()[1].click();
  await p.settle();
  assert.equal(p.book.pushed.pop(), "/threads.html?ws=ws2&new=lead", "another repo: the page goes there");
  assert.deepEqual(opened(), [true, false], "and its details stay shut on the way in");
  assert.deepEqual(p.$("tree").querySelectorAll(".repo")[0].children.map((c) => c.className), ["row open", "side-more"], "a repo the page is not in, with nothing on its list, has no to-do pill");
  rows()[1].click();
  await p.settle();
  assert.deepEqual(opened(), [true, true], "a second click opens them, and both stay open");
  rows()[1].click();
  await p.settle();
  assert.deepEqual(opened(), [true, false], "a click on the repo the page is in closes its details");
  const again = await boot({ workspaces: [app, other], sessions: [lead], storage });
  assert.deepEqual(again.$("tree").querySelectorAll(".row").map((r) => !!r.querySelector(".git-detail")), [true, false], "kept for the next load");
});

test("an agent that runs as a session opens its conversation here, and shows no model picker", async () => {
  const home = { id: "radar", name: "radar", agent: "radar", status: "cold", panes: null };
  const sess = { id: "a1", workspaceId: "radar", kind: "agent", name: "radar", state: "asleep", agentVersion: "1.2.0" };
  const p = await boot({ workspaces: [repo, home], sessions: [sess], windows: [{ id: "w1", name: "ChartLabs", workspaceIds: ["ws1", "radar"] }] });
  const row = p.$("tree").querySelectorAll(".row").find((r) => r.className.includes("agent"));
  assert.equal(row.querySelector(".dot").className, "dot asleep");
  row.click();
  await p.settle();
  assert.equal(p.where(), "", "it does not leave for the terminal lens");
  assert.deepEqual(p.book.pushed.slice(-1), ["/threads.html?s=a1"]);
  assert.equal(p.$("title").textContent, "radar");
  assert.match(p.$("meta").textContent, /ChartLabs · role agent · base 1\.2\.0/);
  assert.equal(p.$("model-box").classList.contains("hidden"), true, "its base names its model");
  assert.equal(p.$("rename").classList.contains("hidden"), true, "and its name");
  assert.equal(p.$("new-thread").classList.contains("hidden"), true, "an agent's home holds no threads: no New thread in its header");
  assert.equal(p.$("composer").classList.contains("hidden"), false, "a message wakes it");
  assert.equal(p.$("input").attrs["aria-label"], "Message this agent");
});

test("a group lists its repos by name, then its agents by name, and an agent opens in the terminal lens", async () => {
  const api = { id: "ws2", name: "api", repoPath: "/p/api" };
  const agent = (id, status) => ({ id, name: id, agent: id, status });
  const p = await boot({ workspaces: [repo, agent("x-post", "cold"), api, agent("radar", "live")], windows: [{ id: "w1", name: "ChartLabs", workspaceIds: ["ws1", "x-post", "ws2", "radar"] }] });
  const rows = p.$("tree").querySelectorAll(".row");
  assert.deepEqual(rows.map((r) => r.querySelector(".row-name").textContent), ["api", "app", "radar", "x-post"]);
  assert.deepEqual(rows.slice(2).map((r) => [r.className, r.querySelector(".dot").className]), [["row agent", "dot live"], ["row agent", "dot asleep"]]);
  const body = p.$("tree").querySelector(".group-body");
  assert.deepEqual(body.children.map((c) => c.className + ":" + (c.classList.contains("side-sub") ? c.textContent : "")).slice(-3), ["side-sub:⚙Agents", "row agent:", "row agent:"], "the agents come under their own heading, after the repos");
  rows[2].click();
  assert.equal(p.where(), "/?w=w1&ws=radar");
  assert.equal(p.posts().length, 0, "opening an agent starts nothing");
});

test("the firehose moves a session's dot and has the lists read again", async () => {
  const p = await boot({ sessions: [lead] });
  const feed = p.book.sockets[0];
  feed.onmessage({ data: JSON.stringify({ t: "attention", workspace: "ws1", pane: "pane1", state: "needs_input", since: 9 }) });
  await p.settle();
  assert.equal(p.$("tree").querySelector(".dot").className, "dot waiting");
  assert.match(p.$("tree").querySelector(".row").className, /\batt-waiting\b/, "a lead that asks something pulses orange on its repo row");
  feed.onmessage({ data: JSON.stringify({ t: "attention", workspace: "ws1", pane: "another-pane", state: "done" }) });
  const reads = () => p.book.calls.filter((c) => c.url === "/v1/sessions" && c.method === "GET").length;
  const before = reads();
  feed.onmessage({ data: JSON.stringify({ t: "sessions-changed", workspace: "ws1" }) });
  p.book.timers.pop().fn();
  await p.settle();
  assert.equal(reads(), before + 1);
  for (const t of ["no-such-kind", "constructor", "__proto__"]) feed.onmessage({ data: JSON.stringify({ t }) });
});

test("a list that could not be read is said, and unsaid by the next read that works", async () => {
  let down = true;
  const p = await boot({ sessions: [lead], routes: [[/^GET \/v1\/sessions$/, () => (down ? { fail: 502, error: "daemon restarting" } : { sessions: [lead] })]] });
  assert.equal(p.$("error-bar").textContent, "/v1/sessions: daemon restarting");
  assert.equal(p.$("error-bar").classList.contains("hidden"), false);
  assert.equal(p.$("tree").querySelectorAll(".row-name").length, 1, "the lists that were read still draw");
  down = false;
  p.book.sockets[0].onmessage({ data: JSON.stringify({ t: "sessions-changed", workspace: "ws1" }) });
  p.book.timers.pop().fn();
  await p.settle();
  assert.equal(p.$("error-bar").classList.contains("hidden"), true);
});

test("the conversation stays pinned to its end, also when a picture loads late, until the reader scrolls up", async () => {
  const p = await boot({ sessions: [lead], search: "?s=lead1" });
  const box = p.$("stream");
  Object.assign(box, { scrollHeight: 1000, clientHeight: 400, scrollTop: 0 });
  await p.frame(snapshot());
  assert.equal(box.scrollTop, 1000, "a conversation just loaded opens at its end");
  box.scrollHeight = 1300; // a picture in it finished loading
  box.onload();
  assert.equal(box.scrollTop, 1300);
  box.scrollTop = 200; // the reader went up to read
  box.onscroll();
  box.scrollHeight = 1600;
  box.onload();
  await p.frame({ t: "event", seq: 6, data: { type: "message.updated", properties: { info: { id: "m1", role: "assistant" } } } });
  assert.equal(box.scrollTop, 200, "and is left where they are");
});

test("a finished turn nobody has looked at pulses green on its repo row, and an idle one does not pulse", async () => {
  const p = await boot({ sessions: [{ ...lead, attention: "done", attentionSince: 77 }] });
  await p.settle();
  assert.match(p.$("tree").querySelector(".row").className, /\batt-done\b/);
  const q = await boot({ sessions: [{ ...lead, attention: "idle" }] });
  assert.doesNotMatch(q.$("tree").querySelector(".row").className, /\batt-/);
});

test("a finished turn on screen is reported as seen, once", async () => {
  const p = await boot({ sessions: [{ ...lead, attention: "done", attentionSince: 77 }], search: "?s=lead1" });
  await p.settle();
  const acted = () => p.posts().filter((c) => c.url === "/v1/sessions/lead1/acted").length;
  assert.equal(acted(), 1);
  p.doc.handlers.visibilitychange();
  await p.settle();
  assert.equal(acted(), 1, "the same claim is not reported twice");
});

// --- a thread with helpers ---

const backendRepo = { id: "ws2", name: "backend", repoPath: "/p/backend", git: { isGitRepo: true, branch: "dev" } };
const threadLead = { id: "t1", workspaceId: "ws1", kind: "", title: "Trade CSV export", cwd: "/wt/app/thread-3", model: "", state: "running", attention: "idle", paneId: "pane-t1", createdAt: 2,
  threadId: 3, role: "lead", repo: "/p/app", worktree: "/wt/app/thread-3", branch: "thread/3-trade-csv-export", name: "app-thread-3" };
const helper = { id: "h1", workspaceId: "ws2", kind: "", title: "Trade CSV export", cwd: "/p/backend", model: "", state: "running", attention: "needs_input", paneId: "pane-h1", createdAt: 3,
  threadId: 3, role: "helper", repo: "/p/backend", readOnly: true, name: "backend-thread-3", goal: "Reading the trades routes." };
const detail = { thread: { id: 3, state: "open", title: "Trade CSV export" }, members: [
  { session: threadLead, repo: "app", status: "1 commit, all committed", changes: { commits: 1, files: [{ path: "src/exportLink.js", added: 1, removed: 0 }] } },
  { session: helper, repo: "backend", status: "Reading only" },
] };
const teamRoutes = [[/^GET \/v1\/threads$/, () => ({ threads: [detail.thread] })], [/^GET \/v1\/threads\/3$/, () => detail]];
// Fresh copies: the page notes attention on the session objects it was given.
const team = (over = {}) => boot({ workspaces: [repo, backendRepo], sessions: [{ ...lead, attention: "idle" }, { ...threadLead }, { ...helper }], search: "?s=t1", ...over, routes: [...(over.routes || []), ...teamRoutes] });

test("a new thread whose first message fails is not made twice by Retry, and a page that moved on stays where it is", async () => {
  let fail = true, release;
  const made = new Promise((r) => { release = r; });
  const p = await boot({ sessions: [lead], search: "?ws=ws1&new=thread", routes: [
    [/^POST \/v1\/threads$/, async () => { await made; return { thread: { id: 3 }, session: { ...threadLead } }; }],
    [/^POST \/v1\/sessions\/t1\/prompt/, () => (fail ? { fail: 500, error: "boom" } : { id: "p2" })],
  ] });
  p.$("input").value = "Add CSV export";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  p.$("tree").querySelectorAll(".row")[0].click(); // away to the lead before the thread exists
  await p.settle();
  const away = p.book.pushed[p.book.pushed.length - 1];
  release();
  await p.settle();
  assert.equal(p.book.pushed[p.book.pushed.length - 1], away, "the new thread does not pull the page back");
  p.$("tree").querySelectorAll(".row").find((r) => r.textContent.includes("Trade CSV export")).click();
  await p.settle();
  const retry = p.$("blocks").querySelector(".sending").querySelectorAll("button").find((b) => b.textContent === "Retry");
  fail = false;
  retry.click();
  await p.settle();
  assert.deepEqual(p.posts().map((c) => c.url), ["/v1/threads", "/v1/sessions/t1/prompt", "/v1/sessions/t1/prompt"], "one thread, the prompt sent to it twice");
  assert.equal(p.$("blocks").querySelector(".sending").textContent, "Sent, waiting for Claude");
});

test("text typed while a new thread is being made moves with it into the thread", async () => {
  let release;
  const made = new Promise((r) => { release = r; });
  const p = await boot({ sessions: [lead], search: "?ws=ws1&new=thread", routes: [
    [/^POST \/v1\/threads$/, async () => { await made; return { thread: { id: 3 }, session: { ...threadLead } }; }],
    [/^POST \/v1\/sessions\/t1\/prompt/, () => ({ id: "p1" })],
  ] });
  p.$("input").value = "Add CSV export";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  p.$("input").value = "and a header row";
  release();
  await p.settle();
  assert.equal(p.$("title").textContent.includes("New thread"), false, "the page moved to the thread");
  assert.equal(p.$("input").value, "and a header row", "what was typed meanwhile is still in the box");
});

test("text typed for a new thread the page left before it was made waits in the thread, not in the repo it went to", async () => {
  let release;
  const made = new Promise((r) => { release = r; });
  const p = await boot({ sessions: [lead], search: "?ws=ws1&new=thread", routes: [
    [/^POST \/v1\/threads$/, async () => { await made; return { thread: { id: 3 }, session: { ...threadLead } }; }],
    [/^POST \/v1\/sessions\/t1\/prompt/, () => ({ id: "p1" })],
  ] });
  p.$("input").value = "Add CSV export";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  p.$("input").value = "and a header row";
  p.$("tree").querySelectorAll(".row")[0].click(); // away to the lead before the thread exists
  await p.settle();
  p.$("input").value = "for the lead";
  release();
  await p.settle();
  assert.equal(p.$("input").value, "for the lead", "the lead's own box is untouched");
  p.$("tree").querySelectorAll(".row").find((r) => r.textContent.includes("Trade CSV export")).click();
  await p.settle();
  assert.equal(p.$("input").value, "and a header row", "the thread's text waited for it");
});

test("a message with a picture uploads it, sends its name with the words, and shows the picture until its turn lands", async () => {
  const p = await boot({ sessions: [lead], search: "?s=lead1" });
  await p.frame(snapshot());
  p.$("file").onchange({ target: { files: [{ type: "image/png", size: 100, name: "shot.png" }], value: "x" } });
  p.$("input").value = "look at this";
  p.$("composer").onsubmit({ preventDefault() {} });
  assert.equal(p.$("attached").children.length, 0, "the composer lets go of the picture at once");
  assert.equal(p.$("blocks").querySelectorAll(".b-user")[1].querySelector("img").src, "blob:x", "drawn from the browser while on its way");
  await p.settle();
  assert.deepEqual(p.posts().map((c) => [c.url, c.body && c.body.images]), [
    ["/v1/sessions/lead1/images", undefined], ["/v1/sessions/lead1/prompt", ["0123456789abcdef0123456789abcdef.png"]],
  ]);
});

test("a new thread is made as a thread, with its lead, by its first message", async () => {
  const p = await boot({ sessions: [lead], search: "?ws=ws1&new=thread", routes: [[/^POST \/v1\/threads$/, (body) => ({ thread: { id: 3 }, session: { ...threadLead, model: body.model } })]] });
  p.$("input").value = "Add CSV export in the app and in backend";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.deepEqual(p.posts().map((c) => [c.url, c.body]), [
    ["/v1/threads", { workspaceId: "ws1", model: "", advisor: "" }],
    ["/v1/sessions/t1/prompt", { text: "Add CSV export in the app and in backend", sender: "Pat", images: [] }],
  ]);
  assert.equal(p.book.pushed.pop(), "/threads.html?s=t1");
});

test("a new thread's advisor is picked before its first message and made with it", async () => {
  const p = await boot({ sessions: [lead], search: "?ws=ws1&new=thread", routes: [[/^POST \/v1\/threads$/, (body) => ({ thread: { id: 3 }, session: { ...threadLead, model: body.model, advisor: body.advisor } })]] });
  assert.equal(p.$("advisor-box").classList.contains("hidden"), false, "beside the model, before it starts");
  p.$("advisor").onchange({ target: { value: "fable" } });
  await p.settle();
  assert.deepEqual(p.posts(), [], "nothing is saved until the thread is made");
  p.$("input").value = "Add CSV export";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.deepEqual(p.posts()[0].body, { workspaceId: "ws1", model: "", advisor: "fable" });
  assert.equal(p.$("model-bar").classList.contains("hidden"), true, "it starts with it: nothing to restart for");
});

test("a thread is filed under its lead's repo with who it works with, and its helper is no thread of the helper's repo", async () => {
  const p = await team();
  const att = (r) => (r.className.match(/att-\w+/) || [""])[0];
  const rows = p.$("tree").querySelectorAll(".row").map((r) => [r.querySelector(".row-name").textContent, r.title, att(r)]);
  assert.deepEqual(rows, [
    ["app", "/p/app\nlead idle", ""],
    ["Trade CSV export", "with backend · needs you", "att-waiting"],
    ["backend", "/p/backend\nno lead yet", ""],
  ], "no second line: the state is a hover tip, and a row that needs you pulses like the lens");
  assert.equal(p.$("tree").querySelectorAll(".row-sub").length, 0);
  assert.equal(p.$("tree").querySelectorAll(".row.thread")[0].querySelector(".dot").className, "dot waiting", "the helper's question shows on the thread");
  assert.equal(p.$("meta").textContent, "ChartLabs · lead: app · helper: backend");
});

test("the Helpers tab lists the team, opens a member, and stands a helper down", async () => {
  const p = await team({ routes: [[/^POST \/v1\/threads\/3\/stand-down$/, () => ({ message: "Stood down backend-thread-3." })]] });
  const members = p.$("tab-team").querySelectorAll(".member");
  assert.deepEqual(members.map((m) => [m.querySelector(".row-name").textContent, m.querySelector(".role").textContent, m.querySelector(".member-doing").textContent, m.querySelector(".member-where").textContent]), [
    ["app", "lead", "Idle.", "worktree · thread/3-trade-csv-export"],
    ["backend", "helper", "Reading the trades routes.", "repo folder · read only"],
  ]);
  assert.equal(members[0].querySelectorAll(".small").length, 0, "the lead is not stood down");
  members[1].querySelector(".small").click();
  await p.settle();
  assert.deepEqual(p.posts().map((c) => [c.url, c.body]), [["/v1/threads/3/stand-down", { repo: "backend" }]]);
  assert.equal(p.$("notice-bar").textContent, "Stood down backend-thread-3.");
  members[1].querySelector(".member-open").click();
  await p.settle();
  assert.equal(p.book.pushed.pop(), "/threads.html?s=h1", "a member's row opens its own conversation");
  assert.equal(p.$("meta").textContent, "ChartLabs · helper in backend · lead: app");
  assert.equal(p.$("merge-thread").classList.contains("hidden"), true, "merging is the lead's");
  assert.equal(p.$("tab-team").querySelectorAll(".small").length, 0, "and so is standing a helper down");
});

test("changes are shown by repo, each with its branch and what it holds", async () => {
  const p = await team();
  const groups = p.$("tab-changes").querySelectorAll(".chg");
  assert.deepEqual(groups.map((g) => [g.querySelector(".chg-repo").textContent, g.querySelector(".branch").textContent, g.querySelector(".chg-status").textContent, g.querySelectorAll(".file-path").map((f) => f.textContent)]), [
    ["app", "thread/3-trade-csv-export", "1 commit, all committed", ["src/exportLink.js"]],
    ["backend", "no branch yet", "Reading only", []],
  ]);
  const plain = await boot({ sessions: [lead], search: "?s=lead1" });
  assert.equal(plain.$("tab-changes").querySelectorAll(".chg").length, 0, "a session in no thread has no team to show");
  assert.equal(plain.$("tab-team").textContent, "No helpers. This session gets one when it asks another repo something.", "a main session gets helpers by asking, not by starting a thread");
  assert.equal(plain.book.calls.some((c) => /\/v1\/threads\/\d/.test(c.url)), false, "and no thread is asked for");
});

test("Merge thread asks the daemon, and says what it said or why it would not", async () => {
  let refuse = true;
  const p = await team({ routes: [[/^POST \/v1\/threads\/3\/merge$/, () => (refuse ? { fail: 409, error: "backend-thread-3 is still working; wait for it to report, then merge" } : { message: "Merging thread #3: app." })]] });
  assert.equal(p.$("merge-thread").classList.contains("hidden"), false);
  p.$("merge-thread").click();
  await p.settle();
  assert.equal(p.$("error-bar").textContent, "backend-thread-3 is still working; wait for it to report, then merge");
  assert.equal(p.$("notice-bar").classList.contains("hidden"), true);
  refuse = false;
  p.$("merge-thread").click();
  await p.settle();
  assert.equal(p.$("notice-bar").textContent, "Merging thread #3: app.");
  assert.equal(p.$("error-bar").classList.contains("hidden"), true);
  // A later refusal does not sit beside the earlier "merging".
  refuse = true;
  p.$("merge-thread").click();
  await p.settle();
  assert.equal(p.$("notice-bar").classList.contains("hidden"), true, "what the earlier merge said is gone");
  assert.equal(p.$("error-bar").textContent, "backend-thread-3 is still working; wait for it to report, then merge");
  p.$("end-thread").click();
  await p.settle();
  assert.equal(p.posts().pop().url, "/v1/threads/3/end");
  const other = await boot({ sessions: [lead], search: "?s=lead1" });
  assert.equal(other.$("merge-thread").classList.contains("hidden"), true, "a repo's own lead leads no thread");
});

test("a running merge shows its step and a Stop, is read again while it runs, and a failed one says where its output is", async () => {
  const merging = { job: "merge-thread-3-ab12cd", unit: "ccmux-job@merge-thread-3-ab12cd.service", state: "running", doing: "backend: checks running", startedAt: Date.now() - 5000, attempts: 1 };
  let now = { ...detail, thread: { ...detail.thread, state: "merging" }, merge: merging };
  const p = await team({ routes: [[/^GET \/v1\/threads\/3$/, () => now], [/^POST \/v1\/threads\/3\/stop-merge$/, () => ({ message: "Stopping the merge of thread #3." })]] });
  assert.equal(p.$("merge-bar").classList.contains("hidden"), false);
  assert.match(p.$("merge-text").textContent, /^Merging · backend: checks running · \d+s$/);
  assert.equal(p.$("merge-bar").className, "bar blue");
  assert.equal(p.$("stop-merge").classList.contains("hidden"), false);
  // Nothing on the feed says which step a job has reached: the page asks again.
  const reads = () => p.book.calls.filter((c) => c.method === "GET" && c.url === "/v1/threads/3").length;
  const before = reads();
  const again = p.book.timers.filter((t) => t.ms === 3000);
  assert.equal(again.length, 1, "one read is waiting, not one per draw");
  now = { ...now, merge: { ...merging, doing: "backend: moving main forward" } };
  again[0].fn();
  await p.settle();
  assert.equal(reads(), before + 1);
  assert.match(p.$("merge-text").textContent, /^Merging · backend: moving main forward · /);
  p.$("stop-merge").click();
  await p.settle();
  assert.equal(p.posts().pop().url, "/v1/threads/3/stop-merge");
  assert.equal(p.$("notice-bar").textContent, "Stopping the merge of thread #3.");
  // It failed on a check: the bar says where the whole output is, and has no Stop.
  now = { ...detail, merge: { ...merging, state: "failed", checkLog: "/wt/backend-b4/thread-3.check.log" } };
  p.book.timers.filter((t) => t.ms === 3000).pop().fn();
  await p.settle();
  assert.equal(p.$("merge-text").textContent, "The last merge failed. The failed check's whole output is in /wt/backend-b4/thread-3.check.log");
  assert.equal(p.$("merge-bar").className, "bar amber");
  assert.equal(p.$("stop-merge").classList.contains("hidden"), true);
  const timers = p.book.timers.filter((t) => t.ms === 3000).length;
  p.$("merge-thread").click();
  await p.settle();
  assert.equal(p.book.timers.filter((t) => t.ms === 3000).length, timers, "a merge that has ended is not polled");
  // A thread with no merge job has no bar; neither has a session in no thread.
  const quiet = await team();
  assert.equal(quiet.$("merge-bar").classList.contains("hidden"), true);
  const plain = await boot({ sessions: [lead], search: "?s=lead1" });
  assert.equal(plain.$("merge-bar").classList.contains("hidden"), true);
});

test("what a thread action said shows beside the session's own notice, and neither hides the other", async () => {
  const noted = { ...threadLead, note: "start failed (1 of 5), trying again: no node" };
  const p = await boot({ workspaces: [repo, backendRepo], sessions: [noted, { ...helper }], search: "?s=t1",
    routes: [[/^POST \/v1\/threads\/3\/stand-down$/, () => ({ message: "Stood down backend-thread-3." })], ...teamRoutes] });
  assert.equal(p.$("notice-bar").textContent, "start failed (1 of 5), trying again: no node");
  p.$("tab-team").querySelector(".small").click();
  await p.settle();
  assert.equal(p.$("notice-bar").textContent, "Stood down backend-thread-3. start failed (1 of 5), trying again: no node");
});

test("a new thread in a folder that is no git repo is a plain session, as it always was", async () => {
  const notes = { id: "ws9", name: "notes", repoPath: "/p/notes", git: { isGitRepo: false } };
  const unread = { id: "ws8", name: "fresh", repoPath: "/p/fresh" }; // its git state is not read yet: not known to be no repo
  const madeIn = async (ws) => {
    const p = await boot({ workspaces: [ws], search: `?ws=${ws.id}&new=thread`, routes: [[/^POST \/v1\/threads$/, () => ({ thread: { id: 3 }, session: { ...threadLead, workspaceId: ws.id } })]] });
    p.$("input").value = "Tidy these notes";
    p.$("composer").onsubmit({ preventDefault() {} });
    await p.settle();
    return p.posts().map((c) => [c.url, c.body])[0];
  };
  assert.deepEqual(await madeIn(notes), ["/v1/sessions", { cwd: "/p/notes", workspaceId: "ws9", model: "", advisor: "" }], "nothing there lands through git, so no thread is asked for");
  assert.deepEqual(await madeIn(unread), ["/v1/threads", { workspaceId: "ws8", model: "", advisor: "" }]);
  assert.deepEqual(await madeIn(repo), ["/v1/threads", { workspaceId: "ws1", model: "", advisor: "" }]);
});

// --- skills and plugins ---

const catalog = {
  capabilities: [
    { id: "qa", kind: "plugin", source: "personal", cost: 1400, version: "1.9.0", commit: "abc1234", pinned: true, updateAvailable: "1.9.2" },
    { id: "chrome-pool", kind: "plugin", source: "personal", cost: 900, version: "2.0.0", commit: "abc1234" },
  ],
  sources: [{ id: "personal", url: "https://github.com/me/claude-personal", commit: "4f2a91c00", updated: new Date().toISOString() }],
};
const capRoutes = (repoChoices) => [
  [/^GET \/v1\/capabilities$/, () => catalog],
  [/^GET \/v1\/capabilities\/choices$/, () => ({ windows: { w1: ["qa"] }, workspaces: {} })],
  [/^GET \/v1\/workspaces\/ws1\/capabilities$/, () => ({ window: "w1", group: ["qa"], choices: repoChoices, effective: repoChoices.qa === "off" ? [] : ["qa"], missing: [] })],
  [/^GET \/v1\/windows\/w1\/capabilities$/, () => ({ on: ["qa"], missing: [] })],
];
// capsTab is this page's tab in the Settings window; capRows reads the rows
// of a panel (the sheet of one repo or group) unless handed the tab.
const capsTab = (p) => p.doc.querySelector(".caps-tab");
const capRows = (p, box = p.$("caps-sheet")) => box.querySelectorAll(".cap-item").map((r) => [r.dataset.cap, (r.querySelector(".cap-meta") || {}).textContent, (r.querySelector(".seg-opt.on") || {}).textContent]);

test("a right-click on a repo opens its menu, and Skills and plugins its panel; Off is sent and read back", async () => {
  const choices = {};
  const p = await boot({ sessions: [lead], routes: [[/^PUT \/v1\/workspaces\/ws1\/capabilities\/qa$/, (body) => { choices.qa = body.state; return {}; }], ...capRoutes(choices)] });
  let prevented = false;
  p.$("tree").querySelector(".row").oncontextmenu({ preventDefault: () => { prevented = true; }, clientX: 40, clientY: 80 });
  assert.equal(prevented, true, "the browser's own menu stays shut");
  assert.equal(p.$("caps-menu").classList.contains("hidden"), false);
  assert.deepEqual(p.$("caps-menu").querySelectorAll(".ctx-item").map((b) => b.textContent), ["Skills and plugins…", "Hostnames…", "Move to group›", "No group (Other)", "New group…"], "a repo in the only group can leave it or make a new one");
  p.$("caps-menu").querySelector(".ctx-item").click();
  await p.settle();
  assert.equal(p.$("caps-menu").classList.contains("hidden"), true);
  assert.equal(p.$("caps-overlay").classList.contains("hidden"), false);
  assert.equal(p.$("caps-sheet").querySelector(".sheet-title").textContent, "app · skills and plugins");
  assert.equal(p.$("caps-sheet").querySelector(".cap-base").textContent, "Always on: CLAUDE.md, memory, the bus, thread limits.");
  assert.deepEqual(capRows(p), [["qa", "On from ChartLabs", "As group"], ["chrome-pool", "Off from ChartLabs", "As group"]]);
  assert.match(p.$("caps-sheet").querySelector(".cap-foot").textContent, /^This repo's sessions load about 1\.4k tokens of these\. Changes apply when a session next starts\.$/);
  p.$("caps-sheet").querySelector('.cap-item[data-cap="qa"] .seg-opt[data-state="off"]').click();
  await p.settle();
  assert.deepEqual(p.book.calls.filter((c) => c.method === "PUT").map((c) => [c.url, c.body]), [["/v1/workspaces/ws1/capabilities/qa", { state: "off" }]]);
  assert.deepEqual(capRows(p)[0], ["qa", "On in ChartLabs, off here", "Off"], "the panel reads its list again");
  assert.match(p.$("caps-sheet").querySelector(".cap-foot").textContent, /load none of these/);
  p.doc.handlers.keydown({ key: "Escape", target: p.doc.body, preventDefault() {} });
  assert.equal(p.$("caps-overlay").classList.contains("hidden"), true, "Escape closes it");
});

test("a read that fails after another sheet was opened leaves that sheet alone", async () => {
  let fail;
  const late = new Promise((resolve) => { fail = resolve; });
  const p = await boot({ sessions: [lead], routes: [[/^GET \/v1\/workspaces\/ws1\/capabilities$/, () => late], ...capRoutes({})] });
  p.$("tree").querySelector(".row").oncontextmenu({ preventDefault() {} });
  p.$("caps-menu").querySelector(".ctx-item").click();
  p.$("tree").querySelector(".group-head .side-more").click();
  p.$("caps-menu").querySelector(".ctx-item").click();
  await p.settle();
  fail({ fail: 500, error: "the repo's read failed" });
  await p.settle();
  assert.equal(p.$("caps-sheet").querySelector(".sheet-title").textContent, "ChartLabs · skills and plugins");
  assert.equal(p.$("caps-sheet").querySelector(".cap-error"), null, "the repo's error is not the group's");
  assert.deepEqual(capRows(p).map((r) => r[2]), ["On", "Off"]);
});

test("an Update that answers after another sheet was opened says nothing on that sheet", async () => {
  let answer;
  const late = new Promise((resolve) => { answer = resolve; });
  const p = await boot({ sessions: [lead], routes: [[/^POST \/v1\/capabilities\/sources\/personal\/update$/, () => late], ...capRoutes({})] });
  p.$("open-settings").click();
  await p.settle();
  capsTab(p).querySelector(".cap-src button").click();
  p.doc.handlers.keydown({ key: "Escape", target: p.doc.body, preventDefault() {} });
  assert.equal(p.$("settings-modal").classList.contains("hidden"), true, "Escape closes the Settings window");
  p.$("tree").querySelector(".group-head .side-more").click();
  p.$("caps-menu").querySelector(".ctx-item").click();
  await p.settle();
  answer({ fail: 502, error: "git fetch failed" });
  await p.settle();
  assert.equal(p.$("caps-sheet").querySelector(".sheet-title").textContent, "ChartLabs · skills and plugins");
  assert.equal(p.$("caps-sheet").querySelector(".cap-error"), null, "the Update's error is not the group's");
  p.$("caps-sheet").querySelector('.cap-item[data-cap="qa"] .seg-opt[data-state="off"]').click();
  await p.settle();
  assert.equal(p.$("caps-sheet").querySelector(".cap-error"), null, "an action on the sheet still open still runs");
});

test("a group's ⋯ opens the group's own panel, on or off", async () => {
  const p = await boot({ sessions: [lead], routes: capRoutes({}) });
  p.$("tree").querySelector(".group-head .side-more").click();
  p.$("caps-menu").querySelector(".ctx-item").click();
  await p.settle();
  assert.equal(p.$("caps-sheet").querySelector(".sheet-title").textContent, "ChartLabs · skills and plugins");
  assert.deepEqual(capRows(p).map((r) => r[2]), ["On", "Off"]);
});

test("the sessions on the old list are named under the panel, and restarted only after a yes", async () => {
  const stale = { ...lead, loadoutStale: true };
  let asked = "";
  const p = await boot({ sessions: [stale], routes: capRoutes({}), confirm: (q) => { asked = q; return true; } });
  p.$("tree").querySelector(".row").oncontextmenu({ preventDefault() {} });
  p.$("caps-menu").querySelector(".ctx-item").click();
  await p.settle();
  assert.equal(p.$("caps-sheet").querySelector(".cap-stale").textContent.trim(), "1 running session uses the old list.");
  assert.equal(p.posts().length, 0, "nothing restarts by itself");
  p.$("caps-sheet").querySelector(".cap-foot .apply").click();
  await p.settle();
  assert.deepEqual(p.posts().map((c) => c.url), ["/v1/sessions/lead1/restart"]);
  assert.equal(p.$("caps-sheet").querySelector(".cap-note").textContent, "Restarting 1 session.");
  assert.equal(asked, "Restart 1 session now? Each stops what it is doing and starts again on the new list.", "the page asks first");
  const no = await boot({ sessions: [stale], routes: capRoutes({}), confirm: () => false });
  no.$("tree").querySelector(".row").oncontextmenu({ preventDefault() {} });
  no.$("caps-menu").querySelector(".ctx-item").click();
  await no.settle();
  no.$("caps-sheet").querySelector(".cap-foot .apply").click();
  await no.settle();
  assert.equal(no.posts().length, 0, "a no restarts nothing");
});

test("a running session whose skills changed says so in its header, with Restart now", async () => {
  const p = await boot({ sessions: [{ ...lead, loadoutStale: true }], search: "?s=lead1" });
  assert.equal(p.$("loadout-bar").classList.contains("hidden"), false);
  p.$("loadout-restart").click();
  await p.settle();
  assert.deepEqual(p.posts().map((c) => c.url), ["/v1/sessions/lead1/restart"]);
  assert.equal(p.$("loadout-bar").classList.contains("hidden"), true, "gone while the restart is on its way");
  const fresh = await boot({ sessions: [lead], search: "?s=lead1" });
  assert.equal(fresh.$("loadout-bar").classList.contains("hidden"), true);
});

test("Settings lists the sources and every entry, and Update says what it pulled", async () => {
  const p = await boot({ sessions: [lead], routes: [[/^POST \/v1\/capabilities\/sources\/personal\/update$/, () => ({ source: { id: "personal" }, changed: [{ id: "chrome-pool" }], added: [] })], ...capRoutes({})] });
  assert.equal(p.$("settings-modal").classList.contains("hidden"), true, "closed until asked for");
  p.$("open-settings").click();
  await p.settle();
  assert.equal(p.$("settings-modal").classList.contains("hidden"), false, "the Settings button opens the one window");
  assert.equal(p.$("caps-overlay").classList.contains("hidden"), true, "and no sheet of this page's own beside it");
  const sheet = capsTab(p);
  assert.equal(sheet.classList.contains("hidden"), false, "this page's own tab is the one on show");
  assert.equal(sheet.querySelector(".cap-where").textContent, "git · me/claude-personal · 4f2a91c · checked today");
  assert.equal(sheet.querySelector(".cap-src .cap-tag").textContent, "1 update", "the pinned qa is behind");
  assert.deepEqual(capRows(p, sheet).map((r) => r[1]), [
    "~1.4k tokens · pinned at 1.9.0 · on in ChartLabs",
    "~0.9k tokens · 2.0.0 · follows newest · on nowhere",
  ]);
  const head = sheet.querySelector(".cap-group-head");
  assert.equal(head.textContent, "›personal2", "the entries sit under their source, with how many");
  assert.equal(sheet.querySelector('.cap-item[data-cap="qa"] .cap-tag').textContent, "update 1.9.0 → 1.9.2");
  head.click();
  assert.equal(capsTab(p).querySelectorAll(".cap-item").length, 0, "a click on the heading folds its section");
  capsTab(p).querySelector(".cap-group-head").click();
  assert.equal(capsTab(p).querySelectorAll(".cap-item").length, 2, "and another unfolds it");
  sheet.querySelector(".cap-src button").click();
  await p.settle();
  assert.equal(p.posts().pop().url, "/v1/capabilities/sources/personal/update");
  assert.equal(capsTab(p).querySelector(".cap-note").textContent, "personal: 1 changed.");
});

test("a folded source stays folded on the next visit, and a stored value that is not a list leaves every source open", async () => {
  const kept = new Map([["ccmux-user", "Pat"]]);
  const storage = { getItem: (k) => (kept.has(k) ? kept.get(k) : null), setItem: (k, v) => kept.set(k, String(v)) };
  const openTab = async (p) => { p.$("open-settings").click(); await p.settle(); return capsTab(p); };
  const first = await boot({ sessions: [lead], storage, routes: capRoutes({}) });
  (await openTab(first)).querySelector(".cap-group-head").click();
  assert.equal(kept.get("ccmux-caps-folded"), '["personal"]');
  const again = await boot({ sessions: [lead], storage, routes: capRoutes({}) });
  const tab = await openTab(again);
  assert.equal(tab.querySelector(".cap-group").className, "cap-group folded", "it opens the way it was left");
  kept.set("ccmux-caps-folded", "5"); // a hand edit, or another page's key
  const odd = await boot({ sessions: [lead], storage, routes: capRoutes({}) });
  assert.equal((await openTab(odd)).querySelectorAll(".cap-item").length, 2, "the list still draws, every source open");
});

test("Notifications on this page: Enable files the device under the page's own name, and a tap stays the page's to follow", async () => {
  const vapid = [/^GET \/v1\/push\/vapid/, () => ({ key: "QUJD" })];
  const p = await boot({ sessions: [lead], push: true, routes: [vapid, ...capRoutes({})] });
  p.$("open-settings").click();
  await p.settle();
  assert.equal(p.$("notif-status").textContent, "Get a push when a session needs you — even with ccmux closed.");
  assert.equal(p.$("notif-enable").classList.contains("hidden"), false);
  assert.equal(p.posts().length, 0, "opening Settings subscribes nothing");
  p.$("notif-enable").click();
  await p.settle();
  const sent = p.posts().filter((c) => c.url.startsWith("/v1/push/subscriptions"));
  assert.deepEqual(sent.map((c) => [c.url, c.body]), [["/v1/push/subscriptions?user=Pat", { endpoint: "https://push.example.invalid/abc", keys: { p256dh: "k", auth: "a" } }]]);
  assert.equal(p.book.sockets[0].url, "ws://d/v1/events?user=Pat&device=web", "the same name the page's presence goes by");
  assert.equal(p.$("notif-status").textContent, "Notifications are on for this device.");
  p.$("notif-disable").click();
  await p.settle();
  assert.deepEqual(p.book.calls.filter((c) => c.method === "DELETE").map((c) => [c.url, c.body]), [["/v1/push/subscriptions", { endpoint: "https://push.example.invalid/abc" }]]);
  assert.equal(p.book.subscription, null);
  // A tap: this page follows it itself (threads.js). push.js, which on the
  // lens reads and clears the stashed tap, must leave the stash alone here.
  assert.equal(p.book.swMessage.length, 1, "one listener for a tap, the page's own");
  p.book.swMessage[0]({ data: { type: "ccmux-navigate", url: "/?ws=ws1" } });
  await p.settle();
  assert.equal(p.where(), "/?ws=ws1");
  assert.equal(p.book.cacheOpens, 0, "the stashed tap is left for a lens to take");
  // A daemon that cannot vouch: the stored name, as on the events socket.
  const plain = await boot({ sessions: [lead], push: true, routes: [vapid, [/^GET \/v1\/whoami/, () => ({ vouched: false, login: "stored-name" })], ...capRoutes({})] });
  plain.$("open-settings").click();
  await plain.settle();
  plain.$("notif-enable").click();
  await plain.settle();
  assert.deepEqual(plain.posts().map((c) => c.url), ["/v1/push/subscriptions?user=stored-name"]);
  assert.equal(plain.book.sockets[0].url, "ws://d/v1/events?user=stored-name&device=web");
});

test("the Settings window is the lens's, whole, with this page's tab in front", async () => {
  const p = await boot({ sessions: [lead], routes: capRoutes({}) });
  const tabs = p.$("settings-tabs").querySelectorAll(".settings-tab");
  assert.deepEqual(tabs.map((t) => t.textContent), ["Skills and plugins", "Accounts", "Harnesses", "Agents", "Tasks", "Sandbox", "Notifications"]);
  p.$("open-settings").click();
  await p.settle();
  const reads = p.book.calls.filter((c) => c.method === "GET").map((c) => c.url);
  for (const url of ["/v1/capabilities", "/v1/settings", "/v1/agents"]) assert.ok(reads.includes(url), `opening reads ${url}`);
  assert.ok(reads.some((u) => u.startsWith("/v1/whoami?user=")), "and asks who this page is, for the Notifications tab");
  assert.equal(p.$("whoami").textContent, "Signed in as Pat (pat@example.invalid).");
  assert.equal(p.$("agent-state").textContent, "No agents yet.", "the Agents tab read its list on a page with no agent menu");
  tabs[1].click();
  assert.equal(capsTab(p).classList.contains("hidden"), true, "picking Accounts puts this page's tab away");
  assert.equal(p.doc.querySelector('section[data-page="accounts"]').classList.contains("hidden"), false);
  p.$("settings-close").click();
  assert.equal(p.$("settings-modal").classList.contains("hidden"), true);
  p.$("tree").querySelector(".group-head .side-more").click();
  p.$("caps-menu").querySelector(".ctx-item").click();
  await p.settle();
  assert.equal(p.$("caps-overlay").classList.contains("hidden"), false, "a group's panel still opens as its own sheet");
  assert.equal(p.$("settings-modal").classList.contains("hidden"), true);
});

// The task queue on the thread page: a Tasks row per group with its counts,
// under each repo the unfinished tasks it has a PART in (one with parts in
// two repos is under both), the queue in place of the conversation, and the
// header's two pickers storing the group's numbers.
test("a group's tasks show under the group and under every repo they have a part in", async () => {
  const blog = { id: "ws2", name: "blog", repoPath: "/p/blog", git: { isGitRepo: true, branch: "main" } };
  const part = (id, repoPath) => ({ id, repoPath, repo: repoPath.split("/").pop(), status: "pending", plan: "p" });
  const tasks = [
    { id: 7, windowId: "w1", title: "Export", status: "waiting", parts: [part(1, "/p/app"), part(2, "/p/blog")], moves: [] },
    { id: 8, windowId: "w1", title: "Old", status: "done", parts: [part(3, "/p/app")], moves: [] },
    { id: 9, windowId: "w1", title: "Fix", status: "needs_you", parts: [part(4, "/p/blog")], moves: [] },
  ];
  const windows = [{ id: "w1", name: "ChartLabs", workspaceIds: ["ws1", "ws2"], taskSlots: 1 }];
  const p = await boot({ workspaces: [repo, blog], windows, sessions: [lead], search: "?s=lead1",
    routes: [[/^GET \/v1\/tasks\?user=Pat$/, () => ({ tasks, counts: { w1: { waiting: 1, done: 1, needs_you: 1 } } })]] });
  const tree = p.$("tree");
  assert.deepEqual(tree.querySelectorAll(".row-name").map((n) => n.textContent), ["app", "blog"], "the Tasks rows are not repos");
  const row = tree.querySelector(".side-tasks");
  assert.equal(row.querySelector(".st-sub").textContent, "1 waiting · 1 done");
  assert.equal(row.querySelector(".st-alert").textContent, "1");
  const under = (name) => tree.querySelectorAll(".repo").find((r) => r.querySelector(".row-name").textContent === name)
    .querySelectorAll(".side-task").map((t) => t.querySelector(".st-id").textContent);
  assert.deepEqual(under("app"), ["#7"], "done tasks are not listed");
  assert.deepEqual(under("blog"), ["#9", "#7"], "the task with a part in blog is under blog too, what needs a person first");

  row.click();
  await p.settle();
  assert.ok(p.$("page").classList.contains("tasks-mode"), "the queue takes the conversation's place");
  assert.ok(!p.$("tasks").classList.contains("hidden"));
  assert.equal(p.$("title").textContent, "Tasks");
  assert.equal(p.book.pushed.pop(), "/threads.html", "the conversation is left");
  assert.ok(p.$("tree").querySelector(".side-tasks").classList.contains("on"));

  p.$("tasks").onchange({ target: { dataset: { act: "priority" }, value: "2" } });
  p.$("tasks").onchange({ target: { dataset: { act: "slots" }, value: "0" } });
  await p.settle();
  const puts = p.book.calls.filter((c) => c.method === "PUT").map((c) => [c.url, c.body]);
  assert.deepEqual(puts, [["/v1/windows/w1/priority", { priority: 2 }], ["/v1/windows/w1/slots", { slots: 0 }]]);
  assert.equal(windows[0].taskPriority, 2, "the page's own window list holds the number at once");

  const reads = () => p.book.calls.filter((c) => c.url.startsWith("/v1/tasks")).length;
  const before = reads();
  p.book.sockets[0].onmessage({ data: JSON.stringify({ t: "tasks-changed" }) });
  await p.settle();
  assert.equal(reads(), before + 1, "a tasks-changed frame reads the queue again");

  tree.querySelectorAll(".repo").find((r) => r.querySelector(".row-name").textContent === "blog").querySelector(".side-task").click();
  await p.settle();
  assert.ok(p.$("tree").querySelectorAll(".side-task").filter((t) => t.classList.contains("on")).length === 2, "blog's tasks are the ones on show");
  assert.ok(!p.$("tree").querySelector(".side-tasks").classList.contains("on"));

  p.$("tree").querySelector(".row").click();
  await p.settle();
  assert.ok(!p.$("page").classList.contains("tasks-mode"), "picking a repo closes the queue");
  assert.ok(p.$("tasks").classList.contains("hidden"));
  assert.equal(p.$("title").textContent, "app");
});

// A part's builder is a session: "Open session" in the queue shows its
// conversation here, in place of the queue. A worker from before builders
// were sessions is a terminal, and still opens in the lens.
test("a task part's builder opens as a conversation on this page, a tmux worker in the lens", async () => {
  const builder = { id: "b1", workspaceId: "ws1", threadId: 9, role: "builder", cwd: "/wt/app/task-7", worktree: "/wt/app/task-7", branch: "task/7-export",
    title: "Task #7: Export", name: "app-task-7", state: "running", attention: "running", paneId: "run-b1", createdAt: 2 };
  const part = (id, more) => ({ id, repoPath: "/p/app", repo: "app", status: "running", plan: "p", workspaceId: "ws1", ...more });
  const tasks = [{ id: 7, windowId: "w1", title: "Export", status: "running", moves: [],
    parts: [part(1, { paneId: "run-b1", sessionId: "b1" }), part(2, { paneId: "pane-old" })] }];
  const windows = [{ id: "w1", name: "ChartLabs", workspaceIds: ["ws1"], taskSlots: 1 }];
  const p = await boot({ workspaces: [repo], windows, sessions: [lead, builder], search: "?s=lead1",
    routes: [[/^GET \/v1\/tasks\?user=Pat$/, () => ({ tasks, counts: { w1: { running: 1 } } })]] });
  assert.ok(p.$("tree").querySelectorAll(".row-name").map((n) => n.textContent).includes("Task #7: Export"), "the builder is a thread row of its repo");
  p.$("tree").querySelector(".side-tasks").click();
  await p.settle();
  assert.ok(p.$("page").classList.contains("tasks-mode"));
  const press = (partId) => {
    const btn = { dataset: { act: "session" }, closest: (sel) => (sel === "[data-part]" ? { dataset: { part: String(partId) } } : null) };
    p.$("tasks").onclick({ target: { closest: (sel) => (sel === "[data-act]" ? btn : sel === ".tq-task" ? { dataset: { id: "7" } } : null) } });
  };
  press(2);
  await p.settle();
  assert.equal(p.book.opened.length, 1, "the tmux worker opens in the lens, in a tab of its own");
  assert.match(p.book.opened[0][0], /ws=ws1/);
  assert.ok(p.$("page").classList.contains("tasks-mode"), "and the queue stays on this page");
  press(1);
  await p.settle();
  assert.equal(p.book.pushed.pop(), "/threads.html?s=b1", "the builder's conversation is picked");
  assert.ok(!p.$("page").classList.contains("tasks-mode"), "in place of the queue");
  assert.equal(p.$("title").textContent, "Task #7: Export");
  assert.equal(p.book.opened.length, 1, "and no tab was opened for it");
});

test("the header shows how full the context is and how long the cache has left, and hides with nothing known", async () => {
  const p = await boot({ sessions: [{ ...lead, attention: "idle" }], search: "?s=lead1" });
  assert.equal(p.$("gauge").classList.contains("hidden"), true);
  const result = { costUSD: 0.5, endedAt: Date.now() - 14 * 60000, cacheTtlMs: 3600000, context: { percent: 42, used: 84000, max: 200000 } };
  await p.frame({ t: "snapshot", boot: "b1", seq: 0, data: { messages: [], info: {}, result } });
  assert.equal(p.$("gauge").classList.contains("hidden"), false);
  assert.equal(p.$("gauge").textContent, "42% · cache 46 min");
  assert.match(p.$("gauge").title, /last reply 14 min ago/);
});

test("a cold cache is a snowflake beside the context share, not crossed-out words", async () => {
  const p = await boot({ sessions: [{ ...lead, attention: "idle" }], search: "?s=lead1" });
  const result = { endedAt: Date.now() - 70 * 60000, cacheTtlMs: 3600000, context: { percent: 42, used: 84000, max: 200000 } };
  await p.frame({ t: "snapshot", boot: "b1", seq: 0, data: { messages: [], info: {}, result } });
  assert.equal(p.$("gauge").textContent, "42% \u2744");
  assert.match(p.$("gauge").title, /expired 10 min ago/);
});

test("the header's status is a dot and a word, and the main session's line is short", async () => {
  const p = await boot({ sessions: [{ ...lead, attention: "idle" }], search: "?s=lead1" });
  assert.equal(p.$("state").className, "status idle");
  assert.equal(p.$("state").textContent, "Idle");
  assert.match(p.$("meta").textContent, / · main session · 0 threads$/);
  assert.match(p.$("model-shown").textContent, /^\S/, "the closed picker names a model");
  assert.equal(p.$("model-shown").textContent.includes("(default)"), false, "and drops (default)");
});

test("while a turn runs the header says the cache is live, not counting down", async () => {
  const p = await boot({ sessions: [{ ...lead, attention: "running" }], search: "?s=lead1" });
  const result = { endedAt: Date.now() - 70 * 60000, cacheTtlMs: 3600000, context: { percent: 42, used: 84000, max: 200000 } };
  await p.frame({ t: "snapshot", boot: "b1", seq: 0, data: { messages: [], info: {}, result, busy: true } });
  assert.equal(p.$("gauge").textContent, "42% · cache live", "a turn that runs past the hour keeps its cache warm");
});

test("the goal line shows only a goal the session wrote, never help text", async () => {
  const p = await boot({ sessions: [{ ...lead, attention: "idle" }], search: "?s=lead1" });
  assert.equal(p.$("goal").classList.contains("hidden"), true, "a lead with no goal has no goal line");
  const q = await boot({ sessions: [{ ...lead, attention: "idle", goal: "Reading the trades routes." }], search: "?s=lead1" });
  assert.equal(q.$("goal").classList.contains("hidden"), false);
  assert.equal(q.$("goal-text").textContent, "Reading the trades routes.");
});

// A repo's to-do list: its row under the repo with how many items wait and
// how urgent the worst is, the list in place of the conversation, and every
// button a person has on an item sent to the daemon as that item's change.
const todoItem = (id, over = {}) => ({ id, repo: "/p/app", text: "item " + id, urgency: "low", status: "open", addedBy: "app", createdAt: id * 1000, updatedAt: id * 1000, ...over });

async function todoPage(todos, opts = {}) {
  const blog = { id: "ws2", name: "blog", repoPath: "/p/blog-worktree", git: { isGitRepo: true, branch: "main" } };
  const state = { todos, confirmed: [] };
  const p = await boot({
    workspaces: [repo, blog], sessions: [lead], search: opts.search === undefined ? "?s=lead1" : opts.search,
    windows: [{ id: "w1", name: "ChartLabs", workspaceIds: ["ws1", "ws2"], taskSlots: 1 }],
    confirm: (q) => { state.confirmed.push(q); return opts.confirm !== false; },
    // blog's session is open on a worktree: the daemon names its list's key.
    routes: [[/^GET \/v1\/todos$/, () => ({ todos: state.todos, repos: { ws1: "/p/app", ws2: "/p/blog" } })], ...(opts.routes || [])],
  });
  const repoBox = (name) => p.$("tree").querySelectorAll(".repo").find((r) => r.querySelector(".row-name").textContent === name);
  const changes = () => p.book.calls.filter((c) => c.url.startsWith("/v1/todos") && c.method !== "GET").map((c) => [c.method, c.url, c.body]);
  const items = () => p.$("repo-todo").querySelectorAll(".rt-item");
  const itemOf = (id) => items().find((i) => i.dataset.id === String(id));
  return { p, state, repoBox, changes, items, itemOf };
}

test("a repo's row shows how many to-do items wait and how urgent the worst is; the list opens in the conversation's place", async () => {
  const { p, repoBox, items } = await todoPage([
    todoItem(1), todoItem(2, { urgency: "high", addedBy: "app-task-7" }), todoItem(3, { urgency: "medium", status: "tasked", taskId: 41 }),
    todoItem(4, { status: "done", doneBy: "Pat", doneAt: 9000 }), todoItem(5, { repo: "/p/blog", urgency: "medium" }),
  ]);
  assert.deepEqual(p.$("tree").querySelectorAll(".row-name").map((n) => n.textContent), ["app", "blog"], "the to-do pills are not repos");
  const appRow = repoBox("app").querySelector(".side-todo");
  assert.equal(appRow.querySelector(".sd-count").textContent, "2", "what a task has, and what is done, does not wait for anyone");
  assert.equal(appRow.querySelector(".sd-count").className, "sd-count u-high");
  assert.equal(appRow.title, "To-do: 1 high · 1 low · 1 tasked", "what they are is in the pill's title");
  const blogRow = repoBox("blog").querySelector(".side-todo");
  assert.equal(blogRow.querySelector(".sd-count").className, "sd-count u-medium", "a session on a worktree shows its repo's list, by the daemon's key");

  appRow.click();
  await p.settle();
  assert.ok(p.$("page").classList.contains("todo-mode"), "the list takes the conversation's place");
  assert.ok(!p.$("repo-todo").classList.contains("hidden"));
  assert.equal(p.$("title").textContent, "To-do");
  assert.match(p.$("meta").textContent, /^app · /);
  assert.equal(p.book.pushed.pop(), "/threads.html", "the conversation is left");
  assert.ok(repoBox("app").querySelector(".side-todo").classList.contains("on"));
  assert.ok(p.$("composer").classList.contains("hidden"), "there is nobody to message on a list");

  assert.deepEqual(items().map((i) => i.dataset.id), ["2", "3", "1", "4"], "high, then medium, then low; the done one last, folded");
  assert.deepEqual(items().map((i) => i.className), ["rt-item u-high s-open", "rt-item u-medium s-tasked", "rt-item u-low s-open", "rt-item s-done"]);
  assert.match(items()[0].querySelector(".rt-meta").textContent, /^#2added by app-task-7 · /);
  assert.equal(items()[1].querySelector(".rt-chip").textContent, "task #41");
  assert.deepEqual(items().map((i) => i.querySelectorAll(".rt-actions button").map((b) => b.textContent)),
    [["Make task", "Done", "Delete"], ["Done", "Delete"], ["Make task", "Done", "Delete"], ["Put back", "Delete"]]);
  const fold = p.$("repo-todo").querySelector(".rt-done");
  assert.equal(fold.open, false, "what is done is folded away");
  assert.equal(fold.querySelector("summary").textContent, "Done (1)");
  assert.match(fold.querySelector(".rt-meta").textContent, /done by Pat/);
});

test("on a to-do item a person changes its urgency, ticks it off, puts it back, deletes it and adds one", async () => {
  const { p, state, repoBox, changes, itemOf } = await todoPage([todoItem(1), todoItem(2, { urgency: "high" }), todoItem(4, { status: "done", doneBy: "Pat", doneAt: 9000 })]);
  repoBox("app").querySelector(".side-todo").click();
  await p.settle();

  const pick = itemOf(1).querySelector(".rt-urgency");
  assert.equal(pick.value, "low");
  pick.value = "high";
  pick.onchange();
  itemOf(2).querySelector(".rt-act-done").click();
  itemOf(4).querySelector(".rt-act-reopen").click();
  await p.settle();
  itemOf(2).querySelector(".rt-act-delete").click();
  await p.settle();
  assert.deepEqual(changes(), [
    ["PATCH", "/v1/todos/1", { urgency: "high" }], ["PATCH", "/v1/todos/2", { status: "done", by: "Pat" }],
    ["PATCH", "/v1/todos/4", { status: "open" }], ["DELETE", "/v1/todos/2", undefined],
  ]);
  assert.deepEqual(state.confirmed, ["Delete to-do #2? It is gone for good."], "only a delete asks first");

  const form = p.$("repo-todo").querySelector(".rt-add");
  const text = form.querySelector(".rt-text");
  await form.onsubmit({ preventDefault() {} });
  assert.equal(changes().length, 4, "an empty line adds nothing");
  text.value = "The export button does nothing on Safari";
  form.querySelector(".rt-urgency").value = "medium";
  await form.onsubmit({ preventDefault() {} });
  await p.settle();
  assert.deepEqual(changes()[4], ["POST", "/v1/todos", { repo: "/p/app", text: "The export button does nothing on Safari", urgency: "medium", by: "Pat" }]);
  assert.equal(text.value, "", "the line is emptied for the next one");
});

test("a delete that is not confirmed deletes nothing, and a refusal is said on the list", async () => {
  const { p, repoBox, changes, itemOf } = await todoPage([todoItem(1)], { confirm: false, routes: [[/^PATCH \/v1\/todos\/1$/, () => ({ fail: 409, error: "the same thing is on the list already as #9" })]] });
  repoBox("app").querySelector(".side-todo").click();
  await p.settle();
  itemOf(1).querySelector(".rt-act-delete").click();
  await p.settle();
  assert.deepEqual(changes(), []);
  itemOf(1).querySelector(".rt-act-done").click();
  await p.settle();
  const status = p.$("repo-todo").querySelector(".rt-status");
  assert.equal(status.textContent, "the same thing is on the list already as #9");
  assert.ok(status.classList.contains("bad"));
});

test("Make task sends the item to its repo's session through the daemon, and an item sent but not filed can be sent again", async () => {
  const { p, state, repoBox, changes, itemOf } = await todoPage([todoItem(1), todoItem(2, { status: "tasked" })]);
  repoBox("app").querySelector(".side-todo").click();
  await p.settle();
  assert.deepEqual(itemOf(2).querySelectorAll(".rt-actions button").map((b) => b.textContent), ["Send again", "Done", "Delete"]);
  assert.equal(itemOf(2).querySelector(".rt-chip").textContent, "sent to the repo's session to file");
  state.todos = [todoItem(1, { status: "tasked" }), todoItem(2, { status: "tasked" })]; // what the daemon answers once the item is sent
  itemOf(1).querySelector(".rt-act-task").click();
  await p.settle();
  assert.deepEqual(changes(), [["POST", "/v1/todos/1/task", { workspaceId: "ws1", sender: "Pat", model: "" }]]);
  assert.equal(p.$("repo-todo").querySelector(".rt-status").textContent, "Sent #1 to app's session to plan and file as a task.");
  assert.equal(itemOf(1).className, "rt-item u-low s-tasked", "the list is read again and shows it tasked");
  assert.equal(p.book.calls.filter((c) => c.method === "POST" && /^\/v1\/sessions/.test(c.url)).length, 0, "the daemon reaches the session; the page starts none");
});

test("a todos-changed frame reads the lists again, and picking a repo or the task queue closes the list", async () => {
  const { p, state, repoBox, items } = await todoPage([todoItem(1)], { routes: [[/^GET \/v1\/tasks/, () => ({ tasks: [], counts: {} })]] });
  repoBox("app").querySelector(".side-todo").click();
  await p.settle();
  const reads = () => p.book.calls.filter((c) => c.method === "GET" && c.url === "/v1/todos").length;
  const before = reads();
  const typed = p.$("repo-todo").querySelector(".rt-text");
  typed.value = "half a thou";
  state.todos = [todoItem(1), todoItem(7, { urgency: "high", addedBy: "app-task-9" })]; // an agent added one meanwhile
  p.book.sockets[0].onmessage({ data: JSON.stringify({ t: "todos-changed" }) });
  await p.settle();
  assert.equal(reads(), before + 1);
  assert.deepEqual(items().map((i) => i.dataset.id), ["7", "1"], "the new item is on the list, in its urgency's place");
  assert.equal(repoBox("app").querySelector(".sd-count").textContent, "2");
  assert.equal(p.$("repo-todo").querySelector(".rt-text"), typed, "the line being typed on is the same element");
  assert.equal(typed.value, "half a thou", "and what was typed is still there");

  p.$("tree").querySelector(".side-tasks").click();
  await p.settle();
  assert.ok(p.$("page").classList.contains("tasks-mode") && !p.$("page").classList.contains("todo-mode"), "the queue takes the list's place");
  assert.ok(p.$("repo-todo").classList.contains("hidden"));
  assert.equal(p.$("title").textContent, "Tasks");
  // With nothing picked, a repo's row is still there while its list has items.
  p.$("tree").querySelectorAll(".repo")[0].querySelector(".side-todo").click();
  await p.settle();
  assert.ok(p.$("page").classList.contains("todo-mode") && !p.$("page").classList.contains("tasks-mode"), "and the list takes the queue's");
  assert.ok(p.$("tasks").classList.contains("hidden"));

  p.$("tree").querySelector(".row").click();
  await p.settle();
  assert.ok(!p.$("page").classList.contains("todo-mode"), "picking a repo closes the list");
  assert.ok(p.$("repo-todo").classList.contains("hidden"));
  assert.equal(p.$("title").textContent, "app");
});

test("the session's own step list is the Steps tab, apart from the repo's to-do list", async () => {
  const { p } = await todoPage([todoItem(1)]);
  await p.frame(snapshot({ todos: [{ id: "1", text: "Write the test", status: "in_progress" }] }));
  assert.equal(p.$("tab-todo"), null, "no tab is called to-do any more");
  assert.deepEqual(p.$("tab-steps").querySelectorAll(".todo-text").map((n) => n.textContent), ["Write the test"]);
  assert.equal(p.$("repo-todo").querySelectorAll(".todo-text").length, 0, "a session's steps are never on the repo's list");
  const html = fs.readFileSync(path.join(__dirname, "threads.html"), "utf8");
  assert.deepEqual([...html.matchAll(/class="tab[^"]*"[^>]*data-tab="(\w+)"[^>]*>([^<]+)</g)].map((m) => [m[1], m[2]]),
    [["changes", "Changes"], ["team", "Helpers"], ["steps", "Steps"]]);
  const empty = await todoPage([]);
  await empty.p.frame(snapshot());
  assert.equal(empty.p.$("tab-steps").textContent, "No step list in this session.");
});

// --- each person's own sidebar (threadsidectl.js) ---

const twoGroups = () => {
  const app = repo, dash = { id: "ws2", name: "shop", repoPath: "/p/shop" };
  const helpMe = { ...lead, id: "d1", workspaceId: "ws2", attention: "needs_input" };
  return {
    workspaces: [app, dash], sessions: [lead, helpMe],
    windows: [{ id: "w1", name: "ChartLabs", workspaceIds: ["ws1"] }, { id: "w2", name: "dasha", workspaceIds: ["ws2"] }],
  };
};
const groupNames = (p) => p.$("tree").querySelectorAll(".group-name").map((n) => n.textContent);
const puts = (p) => p.book.calls.filter((c) => c.method === "PUT").map((c) => [c.url, c.body]);

test("the person's order and folds draw at once: a folded group is its name, pulsing with a count", async () => {
  const mine = [{ windowId: "w2", collapsed: true, position: 0 }];
  const p = await boot({ ...twoGroups(), routes: [[/^GET \/v1\/me\/sidebar/, () => ({ groups: mine })]] });
  assert.ok(p.book.calls.some((c) => c.url === "/v1/me/sidebar?user=Pat"), "asked as the page's own name");
  assert.deepEqual(groupNames(p), ["dasha", "ChartLabs"]);
  const folded = p.$("tree").querySelector(".group.folded");
  assert.equal(folded.dataset.gid, "w2");
  assert.equal(folded.querySelectorAll(".row").length, 0, "its repos are folded away");
  const head = folded.querySelector(".group-head");
  assert.ok(head.classList.contains("att-waiting"), "a session inside needs someone");
  assert.equal(head.querySelector(".group-count").textContent, "1");
  assert.equal(head.querySelector(".group-sum").textContent, "1 repo", "folded, it still says what is inside");
  assert.equal(head.querySelector(".group-fold").textContent, "", "the arrow is a drawn icon, not a text glyph");
  assert.equal(head.querySelector(".group-fold").attrs["aria-expanded"], "false", "threads.css turns the arrow by this");
  assert.equal(folded.querySelector(".group-body"), null, "a folded group has no body");
  assert.equal(folded.children[0], head, "its header is still its first child");
  const open = p.$("tree").querySelector('.group[data-gid="w1"]');
  assert.equal(open.children.map((c) => c.className).join(","), "group-head,group-body", "an open group: its header, then its body");
  assert.equal(open.querySelector(".group-head .group-sum"), null, "open, its repos show themselves: no count");
  assert.equal(open.querySelector(".group-body .repo .row-name").textContent, "app");
  assert.equal(open.querySelector(".side-sub"), null, "a group with no agents has no Agents heading");
  head.querySelector(".group-fold").click();
  await p.settle();
  assert.deepEqual(puts(p), [["/v1/me/sidebar/w2?user=Pat", { collapsed: false }]]);
  assert.equal(p.$("tree").querySelector(".group.folded"), null, "shown at once");
  assert.equal(p.$("tree").querySelectorAll(".row-name").map((n) => n.textContent).join(","), "shop,app");
});

test("a fold that cannot be saved goes back and says so", async () => {
  const p = await boot({ ...twoGroups(), routes: [[/^PUT \/v1\/me\/sidebar\//, () => ({ fail: 503, error: "store down" })]] });
  p.$("tree").querySelector(".group-fold").click();
  await p.settle();
  assert.equal(p.$("tree").querySelector(".group.folded"), null, "back as it was");
  assert.match(p.$("tree").querySelector(".side-note").textContent, /^Could not save the fold \(store down\)/);
});

test("a group's menu moves it and opens it alone; Other has none of that", async () => {
  const p = await boot({ ...twoGroups(), workspaces: [...twoGroups().workspaces, { id: "ws3", name: "loose", repoPath: "/p/loose" }] });
  p.$("tree").querySelectorAll(".group-head .side-more")[0].click();
  const items = () => p.$("caps-menu").querySelectorAll(".ctx-item");
  assert.deepEqual(items().map((b) => b.textContent), ["Skills and plugins…", "Move down", "Open alone in a new window", "Add a repo here…"]);
  items()[1].click();
  await p.settle();
  assert.deepEqual(puts(p), [["/v1/me/sidebar/order?user=Pat", { windowIds: ["w2", "w1"] }]]);
  assert.deepEqual(groupNames(p), ["dasha", "ChartLabs", "Other"], "moved at once");
  p.$("tree").querySelectorAll(".group-head .side-more")[1].click();
  assert.deepEqual(items().map((b) => b.textContent), ["Skills and plugins…", "Move up", "Open alone in a new window", "Add a repo here…"]);
  items()[2].click();
  assert.deepEqual(p.book.opened, [["/threads.html?g=w1", "_blank"]]);
  assert.deepEqual(groupNames(p), ["dasha", "ChartLabs", "Other"], "Other stays last");
  assert.equal(p.$("tree").querySelectorAll(".group-fold").length, 2, "Other has no chevron");
});

test("a page opened for one group shows it alone, unfolded, and keeps to it", async () => {
  const mine = [{ windowId: "w2", collapsed: true, position: 0 }];
  const p = await boot({ ...twoGroups(), search: "?g=w2", routes: [[/^GET \/v1\/me\/sidebar/, () => ({ groups: mine })]] });
  assert.deepEqual(groupNames(p), ["dasha"]);
  assert.equal(p.$("tree").querySelector(".group-fold"), null, "nothing to fold here");
  assert.equal(p.$("tree").querySelectorAll(".row").length, 1, "its repos show though the person folded it");
  assert.equal(p.$("tree").querySelector(".side-alone").href, "/threads.html");
  p.$("tree").querySelector(".group-head .side-more").click();
  assert.deepEqual(p.$("caps-menu").querySelectorAll(".ctx-item").map((b) => b.textContent), ["Skills and plugins…", "Add a repo here…"]);
  p.$("tree").querySelector(".row").click();
  await p.settle();
  assert.equal(p.book.pushed.pop(), "/threads.html?s=d1&g=w2");
});

// --- groups: move a repo, add one ---

const menuOf = (p, name) => {
  const row = p.$("tree").querySelectorAll(".repo .row").find((r) => r.querySelector(".row-name").textContent === name);
  row.oncontextmenu({ preventDefault() {}, clientX: 10, clientY: 10 });
  return p.$("caps-menu");
};
const itemNamed = (box, text) => box.querySelectorAll(".ctx-item").find((b) => b.textContent === text);

test("a repo moves to another group from its menu, after the person says yes, and the lists are read again", async () => {
  const moved = [], asked = [];
  let answer = false;
  const p = await boot({ ...twoGroups(), confirm: (q) => { asked.push(q); return answer; },
    routes: [[/^PUT \/v1\/workspaces\/ws1\/group$/, (body) => { moved.push(body); return {}; }]] });
  const box = menuOf(p, "app");
  assert.deepEqual(box.querySelectorAll(".ctx-item").map((b) => b.textContent), ["Skills and plugins…", "Hostnames…", "Move to group›", "dasha", "No group (Other)", "New group…"]);
  const sub = box.querySelector(".ctx-sub");
  assert.ok(sub.classList.contains("hidden"), "its groups wait behind the arrow");
  itemNamed(box, "Move to group›").click();
  assert.ok(!sub.classList.contains("hidden"), "a click opens them");
  assert.ok(!box.classList.contains("hidden"), "and leaves the menu open");
  itemNamed(box, "dasha").click();
  await p.settle();
  assert.match(asked[0], /^Move app from ChartLabs to dasha\?/);
  assert.match(asked[0], /running sessions restart/);
  assert.deepEqual(moved, [], "no: nothing moves");
  answer = true;
  const reads = p.book.calls.filter((c) => c.url === "/v1/windows").length;
  itemNamed(menuOf(p, "app"), "dasha").click();
  await p.settle();
  assert.deepEqual(moved, [{ group: "dasha" }]);
  assert.equal(p.book.calls.filter((c) => c.url === "/v1/windows").length, reads + 1, "the groups are read again");
  itemNamed(menuOf(p, "app"), "No group (Other)").click();
  await p.settle();
  assert.deepEqual(moved.pop(), { group: "" }, "out of any group");
});

test("a repo moves to a new group by name; an empty name or Other moves nothing and says why", async () => {
  const moved = [];
  let typed = "  Ops ";
  const p = await boot({ ...twoGroups(), prompt: () => typed, routes: [[/^PUT \/v1\/workspaces\/ws1\/group$/, (body) => { moved.push(body); return {}; }]] });
  itemNamed(menuOf(p, "app"), "New group…").click();
  await p.settle();
  assert.deepEqual(moved, [{ group: "Ops" }]);
  for (const bad of ["   ", "other"]) {
    typed = bad;
    itemNamed(menuOf(p, "app"), "New group…").click();
    await p.settle();
  }
  assert.equal(moved.length, 1, "neither moved it (an empty name would have taken it out of its group)");
  assert.match(p.$("error-bar").textContent, /no group are listed/);
  typed = "DASHA";
  itemNamed(menuOf(p, "app"), "New group…").click();
  await p.settle();
  assert.deepEqual(moved.pop(), { group: "dasha" }, "a name the daemon would match goes to that group");
  typed = null;
  itemNamed(menuOf(p, "app"), "New group…").click();
  await p.settle();
  assert.equal(moved.length, 1, "cancel moves nothing");
});

const projects = [[/^GET \/v1\/projects/, () => ({ path: "", projects: [{ name: "svc", path: "/p/svc", git: true }, { name: "lib", path: "/p/lib" }] })]];

test("the sidebar's + adds a folder as a repo in the group on screen, and opens it", async () => {
  const made = [];
  const p = await boot({ ...twoGroups(), search: "?s=lead1",
    routes: [...projects, [/^POST \/v1\/workspaces$/, (body) => { made.push(body); return { id: "ws9" }; }]] });
  assert.ok(p.$("repo-overlay").classList.contains("hidden"));
  p.$("add-repo").click();
  await p.settle();
  assert.ok(!p.$("repo-overlay").classList.contains("hidden"));
  const sheet = p.$("repo-sheet");
  assert.equal(sheet.querySelector(".nr-select").value, "ChartLabs", "the group of the repo on screen");
  assert.deepEqual(sheet.querySelector(".nr-select").children.map((o) => o.textContent), ["No group", "ChartLabs", "dasha", "New group…"]);
  assert.deepEqual(sheet.querySelectorAll(".nr-item").map((r) => r.textContent), ["svcgit+", "lib+"]);
  sheet.querySelector(".nr-add").click();
  await p.settle();
  assert.deepEqual(made, [{ name: "svc", repoPath: "/p/svc", createdBy: "web", startupCommand: "", group: "ChartLabs" }]);
  assert.ok(p.$("repo-overlay").classList.contains("hidden"), "the dialog closes");
  assert.equal(p.book.pushed.pop(), "/threads.html?ws=ws9&new=lead", "and the new repo opens");
});

test("the + dialog makes a new group by name, refuses an empty one, and a group's menu opens it for that group", async () => {
  const made = [];
  const p = await boot({ ...twoGroups(), routes: [...projects, [/^POST \/v1\/workspaces$/, (body) => { made.push(body); return { id: "ws9" }; }]] });
  p.$("tree").querySelectorAll(".group-head .side-more")[1].click();
  itemNamed(p.$("caps-menu"), "Add a repo here…").click();
  await p.settle();
  const sheet = p.$("repo-sheet");
  assert.equal(sheet.querySelector(".nr-select").value, "dasha");
  const sel = sheet.querySelector(".nr-select");
  sel.value = "\n";
  sel.onchange();
  sheet.querySelector(".nr-add").click();
  await p.settle();
  assert.deepEqual(made, [], "no name: nothing made");
  assert.match(sheet.querySelector(".nr-note").textContent, /needs a name/);
  const name = sheet.querySelector(".nr-group .nr-input");
  name.value = "Ops";
  name.oninput();
  sheet.querySelector(".nr-add").click();
  await p.settle();
  assert.equal(made[0].group, "Ops");
});

test("the + dialog goes into a folder, makes one there with git init, and Escape closes it", async () => {
  const made = [], listed = [];
  const p = await boot({ ...twoGroups(), routes: [
    [/^GET \/v1\/projects/, () => { listed.push(p.book.calls[p.book.calls.length - 1].url); return { path: listed.length > 1 ? "code" : "", parent: "", projects: [{ name: "code", path: "/p/code" }] }; }],
    [/^POST \/v1\/projects$/, (body) => { made.push(body); return {}; }],
  ] });
  p.$("add-repo").click();
  await p.settle();
  const sheet = p.$("repo-sheet");
  sheet.querySelector(".nr-name").click();
  await p.settle();
  assert.equal(listed[1], "/v1/projects?path=code");
  assert.equal(sheet.querySelector(".nr-path").textContent, "/code");
  const folder = sheet.querySelector(".nr-folder .nr-input");
  folder.value = "new-svc";
  folder.oninput();
  sheet.querySelector(".nr-folder .primary").click();
  await p.settle();
  assert.deepEqual(made, [{ path: "code/new-svc", git: true }]);
  assert.equal(listed[2], "/v1/projects?path=code", "the folder is listed again");
  p.doc.dispatchEvent({ type: "keydown", key: "Escape" });
  assert.ok(p.$("repo-overlay").classList.contains("hidden"));
});

// --- an agent's earlier conversations ---

test("an agent's Earlier lists its conversations, shows one read only, and Back to now returns", async () => {
  const home = { id: "radar", name: "radar", agent: "radar", status: "cold", panes: null };
  const sess = { id: "a1", workspaceId: "radar", kind: "agent", name: "radar", state: "running", agentVersion: "1.2.0" };
  const old = { t: "snapshot", state: "past", data: { messages: [{ info: { id: "o1", role: "user" }, parts: [{ id: "o1.0", messageID: "o1", type: "text", text: "last week's brief" }] }] } };
  const reads = [];
  const p = await boot({
    workspaces: [repo, home], sessions: [sess], search: "?s=a1", windows: [{ id: "w1", name: "ChartLabs", workspaceIds: ["ws1", "radar"] }],
    routes: [
      [/^GET \/v1\/sessions\/a1\/conversations$/, () => ({ current: "c3", conversations: [{ id: "c3", title: "today", updated: Date.UTC(2026, 10, 20, 8), current: true }, { id: "c2", title: "the weekly brief", updated: Date.UTC(2026, 10, 13, 8) }] })],
      [/^GET \/v1\/sessions\/a1\/conversations\/c2$/, () => { reads.push("c2"); return old; }],
    ],
  });
  await p.frame(snapshot());
  const bubbles = () => p.$("blocks").querySelectorAll(".bubble").map((b) => b.textContent);
  assert.deepEqual(bubbles(), ["hello there"]);
  assert.equal(p.$("past").classList.contains("hidden"), false, "an agent's header has Earlier");
  p.$("past").click();
  await p.settle();
  const items = p.$("caps-menu").querySelectorAll(".ctx-item");
  assert.equal(items.length, 2);
  assert.match(items[0].textContent, /today \(now\)$/);
  assert.match(items[1].textContent, /^2026-11-13 \d\d:\d\d · the weekly brief$/);
  items[1].click();
  await p.settle();
  assert.deepEqual(reads, ["c2"]);
  assert.deepEqual(bubbles(), ["last week's brief"], "the earlier conversation takes the current one's place");
  assert.equal(p.$("past-bar").classList.contains("hidden"), false);
  assert.match(p.$("past-text").textContent, /^An earlier conversation, from 2026-11-13/);
  assert.equal(p.$("composer").classList.contains("hidden"), true, "it is read only");
  await p.frame({ t: "event", seq: 6, data: { type: "message.updated", properties: { info: { id: "p1", role: "user" } } } });
  await p.frame({ t: "event", seq: 7, data: { type: "message.part.updated", properties: { part: { id: "p1.0", messageID: "p1", type: "text", text: "live words" } } } });
  assert.deepEqual(bubbles(), ["last week's brief"], "what the agent says now does not land in the old conversation");
  p.$("past-back").click();
  await p.settle();
  assert.equal(p.$("past-bar").classList.contains("hidden"), true);
  assert.equal(p.$("composer").classList.contains("hidden"), false);
  assert.deepEqual(bubbles(), ["hello there", "live words"], "back to the current conversation, with what came meanwhile");
});

test("a repo's lead has no Earlier", async () => {
  const p = await boot({ sessions: [lead], search: "?s=lead1" });
  assert.equal(p.$("past").classList.contains("hidden"), true);
});

// --- a repo's Hostnames ---

test("Hostnames opens from a repo's menu with what listens, maps a port, saves the names and reads the lists again", async () => {
  const saved = [];
  const ws = { ...repo, hostnames: [{ name: "app", port: 3000, url: "https://app.example.invalid", listening: true }] };
  const p = await boot({ workspaces: [ws], routes: [
    [/^GET \/v1\/workspaces\/ws1\/port-suggestions$/, () => ({ listening: [{ port: 3000, process: "node" }, { port: 5173, process: "vite" }] })],
    [/^PUT \/v1\/workspaces\/ws1\/hostnames$/, (body) => { saved.push(body); return {}; }],
  ] });
  const box = menuOf(p, "app");
  assert.deepEqual(box.querySelectorAll(".ctx-item").map((b) => b.textContent).slice(0, 3), ["Skills and plugins…", "Hostnames…", "● app : 3000"], "a saved name opens from the menu");
  itemNamed(box, "● app : 3000").click();
  assert.deepEqual(p.book.opened.at(-1), ["https://app.example.invalid", "_blank"]);
  menuOf(p, "app");
  itemNamed(box, "Hostnames…").click();
  await p.settle();
  const sheet = p.$("hosts-sheet");
  assert.ok(!p.$("hosts-overlay").classList.contains("hidden"));
  assert.equal(sheet.querySelector(".sheet-title").textContent, "app · hostnames");
  const live = () => sheet.querySelectorAll(".hn-live-row").map((li) => li.children.map((c) => c.textContent));
  assert.deepEqual(live(), [["3000", "node", "mapped"], ["5173", "vite", "map"]]);
  sheet.querySelectorAll(".hn-live-row")[1].querySelector(".hn-link").click();
  assert.deepEqual(sheet.querySelectorAll(".hn-row").map((r) => [r.querySelector(".hn-name").value, r.querySelector(".hn-port").value]), [["app", "3000"], ["app-2", "5173"]]);
  const reads = p.book.calls.filter((c) => c.url === "/v1/workspaces").length;
  sheet.querySelector(".hn-actions .primary").click();
  await p.settle();
  assert.deepEqual(saved, [{ hostnames: [{ name: "app", port: 3000 }, { name: "app-2", port: 5173 }] }], "no devCommand: a save here keeps the saved one");
  assert.ok(p.$("hosts-overlay").classList.contains("hidden"));
  assert.ok(p.book.calls.filter((c) => c.url === "/v1/workspaces").length > reads, "the lists are read again");
});

test("Hostnames says why a save was refused and stays open", async () => {
  const p = await boot({ routes: [
    [/^GET \/v1\/workspaces\/ws1\/port-suggestions$/, () => ({ fail: 500, error: "down" })],
    [/^PUT \/v1\/workspaces\/ws1\/hostnames$/, () => ({ fail: 409, error: "app is held by blog" })],
  ] });
  itemNamed(menuOf(p, "app"), "Hostnames…").click();
  await p.settle();
  const sheet = p.$("hosts-sheet");
  assert.match(sheet.querySelector(".hn-note").textContent, /Could not read/, "a failed read is not 'nothing listens'");
  const save = () => sheet.querySelector(".hn-actions .primary").click();
  const row = sheet.querySelector(".hn-row");
  row.querySelector(".hn-name").value = "app";
  row.querySelector(".hn-name").oninput();
  row.querySelector(".hn-port").value = "70000";
  row.querySelector(".hn-port").oninput();
  save();
  assert.match(sheet.querySelector(".hn-error").textContent, /1 to 65535/);
  row.querySelector(".hn-port").value = "80";
  row.querySelector(".hn-port").oninput();
  save();
  await p.settle();
  assert.equal(sheet.querySelector(".hn-error").textContent, "Saving failed: app is held by blog");
  assert.ok(!p.$("hosts-overlay").classList.contains("hidden"));
});

// --- Edit: going back to a sent message ---

test("Edit on a sent message goes back to it, after a yes, and puts its words back in the box", async () => {
  const asked = [];
  const p = await boot({ sessions: [lead], search: "?s=lead1", confirm: (q) => { asked.push(q); return true; }, routes: [
    [/^POST \/v1\/sessions\/lead1\/rewind$/, (body) => (body.dry ? { dropped: 1, files: { canRewind: true, files: ["/p/app/a.txt"] } } : { dropped: 1, files: { canRewind: true }, conversation: "f1" })],
  ] });
  await p.frame(snapshot());
  const edit = p.$("blocks").querySelector(".b-user .b-edit");
  assert.ok(edit, "a sent message has Edit");
  edit.click();
  await p.settle();
  assert.deepEqual(p.book.calls.filter((c) => c.url.endsWith("/rewind")).map((c) => c.body), [{ to: "u1", dry: true }, { to: "u1" }]);
  assert.match(asked[0], /put back \(1\): a\.txt/);
  assert.equal(p.$("input").value, "hello there");
  assert.match(p.$("notice-bar").textContent, /Went back/);
});

// --- the predicted next prompt ---

test("Claude's predicted next message shows in the empty box, Tab or the button fills it in, and typing hides it", async () => {
  const p = await boot({ sessions: [lead], search: "?s=lead1" });
  await p.frame(snapshot({ suggestion: "run the tests" }));
  const input = p.$("input");
  assert.equal(input.placeholder, "run the tests");
  assert.ok(!p.$("suggest").classList.contains("hidden"));
  let prevented = false;
  input.onkeydown({ key: "Tab", preventDefault: () => { prevented = true; } });
  assert.equal(input.value, "run the tests", "Tab takes it");
  assert.equal(prevented, true, "and does not move the focus");
  assert.equal(input.placeholder, "", "typed now, so no grey text");
  assert.ok(p.$("suggest").classList.contains("hidden"));
  input.value = "";
  input.oninput();
  p.$("suggest").click();
  assert.equal(input.value, "run the tests", "the button does the same, for a phone");
  input.value = "";
  input.oninput();
  prevented = false;
  await p.frame({ t: "event", seq: 6, data: { type: "session.suggestion", properties: { suggestion: "" } } });
  input.onkeydown({ key: "Tab", preventDefault: () => { prevented = true; } });
  assert.equal(prevented, false, "with nothing offered, Tab moves the focus as always");
  assert.equal(input.placeholder, "");
});

// --- what a running session does besides talking ---

test("a running session's mode switches from the header, its background tasks stop from the bar, and a near limit warns", async () => {
  const asked = [];
  const p = await boot({ sessions: [{ ...lead, state: "running" }], search: "?s=lead1", routes: [
    [/^POST \/v1\/sessions\/lead1\/(mode|tasks\/b1\/stop)$/, (body) => { asked.push(body || "stop"); return {}; }],
  ] });
  await p.frame(snapshot({ info: { permissionMode: "default", tasks: [{ id: "b1", type: "local_bash", description: "npm run dev" }],
    limit: { status: "allowed_warning", type: "five_hour", utilization: 0.9 } } }));
  assert.ok(!p.$("mode-box").classList.contains("hidden"));
  assert.equal(p.$("mode").value, "default");
  p.$("mode").value = "plan";
  p.$("mode").onchange({ target: p.$("mode") });
  await p.settle();
  assert.deepEqual(asked, [{ mode: "plan" }]);
  const one = p.$("bgtasks-bar").querySelector(".bg-head");
  assert.deepEqual([one.querySelector(".bg-kind").textContent, one.querySelector(".bg-name").textContent], ["Shell", "npm run dev"], "one task: its kind and words on the one line, with its Stop");
  p.$("bgtasks-bar").querySelector(".bg-stop").click();
  await p.settle();
  assert.deepEqual(asked, [{ mode: "plan" }, "stop"]);
  assert.match(p.$("limit-bar").textContent, /near your 5-hour usage limit \(90% used\)/);
  await p.frame({ t: "event", seq: 6, data: { type: "session.info", properties: { info: { permissionMode: "plan", tasks: [], limit: null } } } });
  assert.equal(p.$("mode").value, "plan");
  assert.ok(p.$("bgtasks-bar").classList.contains("hidden"), "no tasks, no bar");
  assert.ok(p.$("limit-bar").classList.contains("hidden"));
});

test("several background tasks fold into one line that opens to a row each, every row with its Stop", async () => {
  const p = await boot({ sessions: [{ ...lead, state: "running" }], search: "?s=lead1" });
  const agent = (id, description) => ({ id, type: "local_agent", agent: "qa:" + id, description, since: Date.now() - 12 * 60000 });
  await p.frame(snapshot({ info: { tasks: [agent("a1", "Code review"), agent("a2", "Silent-failure review"), agent("a3", "Test coverage")] } }));
  const bar = p.$("bgtasks-bar");
  assert.equal(bar.querySelector(".bg-head").textContent, "3 running · Code review, Silent-failure review, +1⌃", "folded: one line");
  assert.equal(bar.querySelectorAll(".bg-row").length, 0);
  bar.querySelector(".bg-head").click();
  const rows = p.$("bgtasks-bar").querySelectorAll(".bg-row");
  assert.deepEqual(rows.map((r) => [r.querySelector(".bg-name").textContent, r.querySelector(".bg-time").textContent]), [
    ["Code review · qa:a1", "12m"], ["Silent-failure review · qa:a2", "12m"], ["Test coverage · qa:a3", "12m"],
  ]);
  rows[1].querySelector(".bg-stop").click();
  await p.settle();
  assert.deepEqual(p.posts().map((c) => c.url), ["/v1/sessions/lead1/tasks/a2/stop"], "Stop on a row stops that task");
  p.$("bgtasks-bar").querySelector(".bg-head").click();
  assert.equal(p.$("bgtasks-bar").querySelectorAll(".bg-row").length, 0, "and the line folds again");
  assert.equal(p.doc.activeElement, p.$("bgtasks-bar").querySelector(".bg-head"), "the keyboard stays on the line it pressed");
  p.$("bgtasks-bar").querySelector(".bg-head").click();
  const stop = p.$("bgtasks-bar").querySelectorAll(".bg-row")[0].querySelector(".bg-stop");
  const time = p.$("bgtasks-bar").querySelectorAll(".bg-row")[0].querySelector(".bg-time");
  time.textContent = "stale";
  p.book.intervals.find((i) => i.ms === 30000).fn(); // the page's minute hand
  assert.equal(time.textContent, "12m", "the minutes move on");
  assert.equal(p.$("bgtasks-bar").querySelectorAll(".bg-row")[0].querySelector(".bg-stop"), stop, "and nothing is rebuilt: focus and a Stop on its way stay put");
});

test("the tray is not rebuilt while its tasks are the same: a Stop on its way stays disabled through the lists' reload", async () => {
  let release;
  const p = await boot({ sessions: [{ ...lead, state: "running" }], search: "?s=lead1", routes: [
    [/^POST \/v1\/sessions\/lead1\/tasks\/b1\/stop$/, () => new Promise((r) => { release = () => r({}); })],
  ] });
  const task = { id: "b1", type: "local_bash", description: "npm run dev", since: Date.now() - 3 * 60000 };
  await p.frame(snapshot({ info: { tasks: [task] } }));
  const stop = p.$("bgtasks-bar").querySelector(".bg-stop");
  stop.click();
  await p.settle();
  assert.equal(stop.disabled, true, "the Stop is on its way");
  p.book.intervals.find((i) => i.ms === 20000).fn(); // the lists' reload
  await p.settle();
  await p.frame({ t: "event", seq: 6, data: { type: "session.info", properties: { info: { tasks: [{ ...task }] } } } });
  assert.equal(p.$("bgtasks-bar").querySelector(".bg-stop"), stop, "the same tasks: nothing is rebuilt");
  assert.equal(stop.disabled, true, "so a second Stop cannot be sent");
  release();
  await p.settle();
  assert.equal(stop.disabled, false);
  await p.frame({ t: "event", seq: 7, data: { type: "session.info", properties: { info: { tasks: [task, { id: "b2", type: "monitor", description: "tail log" }] } } } });
  assert.equal(p.$("bgtasks-bar").querySelector(".bg-head .bg-what").textContent, "npm run dev, tail log", "another task: the tray is drawn anew");
  await p.frame({ t: "event", seq: 8, data: { type: "session.info", properties: { info: { tasks: [] } } } });
  assert.ok(p.$("bgtasks-bar").classList.contains("hidden"), "no tasks, no bar");
});

test("typing @ lists the session's matching files, and a pick puts the path in the box", async () => {
  const p = await boot({ sessions: [lead], search: "?s=lead1", routes: [
    [/^GET \/v1\/sessions\/lead1\/files\?q=he$/, () => ({ files: ["src/head.go", "README.md"] })],
  ] });
  await p.frame(snapshot());
  const input = p.$("input");
  input.value = "look at @he";
  input.selectionStart = input.value.length;
  input.oninput();
  p.book.timers.pop().fn(); // the pause after typing
  await p.settle();
  const rows = p.$("mentions").querySelectorAll(".cmd");
  assert.deepEqual(rows.map((r) => r.textContent), ["@src/head.go", "@README.md"]);
  let prevented = false;
  input.onkeydown({ key: "Enter", preventDefault: () => { prevented = true; } });
  assert.equal(prevented, true, "Enter picks; it does not send");
  assert.equal(input.value, "look at @src/head.go ");
  assert.ok(p.$("mentions").classList.contains("hidden"));
  assert.equal(p.book.calls.filter((c) => c.url.endsWith("/prompt")).length, 0);
});

test("an @ list that cannot be read says why on the page", async () => {
  const p = await boot({ sessions: [lead], search: "?s=lead1", routes: [
    [/^GET \/v1\/sessions\/lead1\/files/, () => ({ fail: 502, error: "the files could not be listed: git ls-files: exit status 128: fatal: detected dubious ownership" })],
  ] });
  await p.frame(snapshot());
  const input = p.$("input");
  input.value = "@he";
  input.selectionStart = 3;
  input.oninput();
  p.book.timers.pop().fn();
  await p.settle();
  assert.match(p.$("error-bar").textContent, /Files to mention could not be listed: .*dubious ownership/);
  assert.ok(p.$("mentions").classList.contains("hidden"));
});

test("a message on a cold cache asks first: Start clean sends its key, Cancel sends nothing", async () => {
  const p = await boot({ sessions: [lead], search: "?s=lead1", routes: [[/^POST \/v1\/sessions\/lead1\/prompt/, () => ({ id: "p1" })]] });
  const result = { endedAt: Date.now() - 2 * 3600000, cacheTtlMs: 3600000, context: { used: 84000, max: 200000, percent: 42 } };
  await p.frame(snapshot({ result }));
  const box = p.$("clean-ask");
  let pick = "", asked = 0;
  box.showModal = () => { asked++; box.returnValue = pick; box.onclose(); };
  pick = "";
  p.$("input").value = "not yet";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.equal(asked, 1);
  assert.match(p.$("clean-text").textContent, /re-reads 84k tokens \(42% of the context window\)/);
  assert.deepEqual(p.posts(), [], "Cancel sends nothing");
  assert.equal(p.$("input").value, "not yet", "and keeps the words");
  pick = "clean";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.equal(p.posts().length, 1);
  assert.match(p.posts()[0].body.clean, /^c[0-9a-z]+$/, "the message carries its clean-start key");
  assert.equal(p.posts()[0].body.text, "not yet");
  pick = "continue";
  p.$("input").value = "go on";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.equal(p.posts()[1].body.clean, undefined, "Continue sends it plainly");
});

test("the conversations a clean start left show above the one it runs now, read only, each ending in a divider", async () => {
  const at = Date.now() - 60000;
  const moved = { ...lead, conversation: "c2", leftConversations: [{ id: "c1", at }] };
  const old = snapshot({ messages: [{ info: { id: "o1", role: "user" }, parts: [{ id: "o1.0", messageID: "o1", type: "text", text: "earlier words" }] }] });
  const p = await boot({ sessions: [moved], search: "?s=lead1", routes: [[/^GET \/v1\/sessions\/lead1\/conversations\/c1$/, () => ({ ...old, state: "past" })]] });
  await p.frame(snapshot());
  await p.settle();
  const blocks = p.$("blocks");
  assert.deepEqual(blocks.children.map((b) => b.className), ["b-user", "b-divider", "b-user"]);
  assert.deepEqual(blocks.querySelectorAll(".bubble").map((b) => b.textContent), ["earlier words", "hello there"]);
  assert.match(blocks.querySelector(".b-divider").textContent, /^New clean conversation, started \d\d:\d\d\./);
  assert.equal(blocks.children[0].querySelector(".b-edit"), null, "an earlier conversation's message has no Edit");
  assert.notEqual(blocks.children[2].querySelector(".b-edit"), null, "the current one's does");
});

// --- a main session's side helpers ---

test("a main session's Helpers tab says its helpers end by themselves, and standing one down warns that its scratch copy goes", async () => {
  const asked = [];
  const sideHelper = { id: "sh1", workspaceId: "ws2", kind: "", title: "Helping app", cwd: "/wt/backend/thread-9", model: "", state: "running", attention: "done", paneId: "pane-sh1", createdAt: 4,
    threadId: 9, role: "helper", repo: "/p/backend", worktree: "/wt/backend/thread-9", branch: "thread/9-x", name: "backend-for-app" };
  const side = { id: 9, kind: "side", leadSession: "lead1", state: "open" };
  const p = await boot({ workspaces: [repo, backendRepo], sessions: [{ ...lead }, sideHelper], search: "?s=lead1", confirm: (q) => { asked.push(q); return true; }, routes: [
    [/^GET \/v1\/threads$/, () => ({ threads: [side] })],
    [/^GET \/v1\/threads\/9$/, () => ({ thread: side, members: [{ session: sideHelper, repo: "backend", status: "No changes yet" }] })],
    [/^POST \/v1\/threads\/9\/stand-down$/, () => ({ message: "Stood down backend-for-app." })],
  ] });
  await p.settle();
  const tab = p.$("tab-team");
  assert.match(tab.querySelector(".quiet").textContent, /ends by itself an hour after its last answer/);
  const members = tab.querySelectorAll(".member");
  assert.equal(members.length, 2, "the main session and its helper");
  members[1].querySelector(".small").click();
  await p.settle();
  assert.deepEqual(asked, ["Stand the backend helper down? Its scratch copy is deleted; nothing in it is kept."]);
  assert.deepEqual(p.posts().map((c) => [c.url, c.body]), [["/v1/threads/9/stand-down", { repo: "backend" }]]);
});

test("a thread's Helpers tab keeps the lead's wording, and standing a helper down says its uncommitted work stays", async () => {
  const asked = [];
  const p = await team({ confirm: (q) => { asked.push(q); return true; }, routes: [[/^POST \/v1\/threads\/3\/stand-down$/, () => ({ message: "ok" })]] });
  assert.equal(p.$("tab-team").querySelector(".quiet").textContent, "Helpers this thread stood up. The lead stands them down when it is done.");
  p.$("tab-team").querySelectorAll(".member")[1].querySelector(".small").click();
  await p.settle();
  assert.deepEqual(asked, ["Stand the backend helper down? Work it has not committed stays in its worktree."]);
});

// --- Start over ---

test("Start over asks the daemon for a new conversation, and a typed /clear does the same instead of going to Claude Code", async () => {
  const p = await boot({ sessions: [lead], search: "?s=lead1", routes: [[/^POST \/v1\/sessions\/lead1\/start-over$/, () => ({ startedOver: "lead1" })]] });
  await p.frame(snapshot());
  assert.equal(p.$("start-over").classList.contains("hidden"), false, "a running session can start over");
  p.$("start-over").click();
  await p.settle();
  p.$("input").value = "  /clear ";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.deepEqual(p.posts().map((c) => c.url), ["/v1/sessions/lead1/start-over", "/v1/sessions/lead1/start-over"], "no prompt is sent");
  assert.equal(p.$("input").value, "", "the composer is emptied");
  assert.equal(p.$("notice-bar").textContent, "Started a new conversation. The last one is kept above.");
  await p.frame({ t: "snapshot", boot: "b2", seq: 0, data: { messages: [], info: {} } }); // the runner comes back on the new conversation
  assert.equal(p.$("restarted-bar").classList.contains("hidden"), true, "no restart bar: it did not pick up where it was");
  p.$("input").value = "/clear the redis cache, please";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.deepEqual(p.posts().map((c) => c.url).slice(2), ["/v1/sessions/lead1/prompt"], "words after /clear are a message");
});

test("an agent's /clear is not sent: it is told how an agent starts fresh, and it has no Start over", async () => {
  const agentSess = { ...lead, id: "ag1", kind: "agent", paneId: "pane-ag1" };
  const p = await boot({ sessions: [agentSess], search: "?s=ag1" });
  await p.frame(snapshot());
  assert.equal(p.$("start-over").classList.contains("hidden"), true);
  p.$("input").value = "/clear";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.deepEqual(p.posts(), [], "nothing reaches the agent");
  assert.match(p.$("error-bar").textContent, /put to sleep/);
});
