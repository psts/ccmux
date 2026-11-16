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
  set innerHTML(_) { /* a fixed icon: nothing a test reads */ }
  get firstChild() { return this.children[0] || null; }
  get nextSibling() { const sibs = this.parentNode ? this.parentNode.children : []; return sibs[sibs.indexOf(this) + 1] || null; }
  get childElementCount() { return this.children.filter((c) => c.tagName !== "#TEXT").length; }
  get isConnected() { return true; }
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
  };
  for (const m of html.matchAll(/<(\w+)([^>]*\bid="([^"]+)"[^>]*)>/g)) {
    const el = doc.createElement(m[1]);
    el.id = m[3];
    el.className = (/\bclass="([^"]*)"/.exec(m[2]) || [, ""])[1];
    byId.set(el.id, el);
    body.appendChild(el);
  }
  byId.get("composer").appendChild(doc.createElement("label"));
  return doc;
}

// --- the fake daemon and browser ---

const scripts = [...fs.readFileSync(path.join(__dirname, "threads.html"), "utf8").matchAll(/<script src="\/([^"]+)"/g)].map((m) => m[1]).filter((f) => f !== "build.js" && f !== "update.js");

const repo = { id: "ws1", name: "app", repoPath: "/p/app", git: { isGitRepo: true, branch: "dev" } };
const lead = { id: "lead1", workspaceId: "ws1", kind: "lead", cwd: "/p/app", model: "", state: "running", attention: "idle", paneId: "pane1", createdAt: 1 };

async function boot(opts = {}) {
  const doc = page();
  const book = { calls: [], sockets: [], timers: [], frames: [], pushed: [] };
  const lists = { windows: [{ id: "w1", name: "ChartLabs", workspaceIds: ["ws1"] }], workspaces: [repo], sessions: opts.sessions || [] };
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
    const out = hit ? hit[1](body) : {};
    if (out && out.fail) return { ok: false, status: out.fail, json: async () => ({ error: out.error }) };
    return { ok: true, status: 200, json: async () => out };
  };
  const ctx = {
    console, document: doc, fetch, URLSearchParams, URL: Object.assign(function (...a) { return new URL(...a); }, { createObjectURL: () => "blob:x", revokeObjectURL() {} }), Blob,
    location: { search: opts.search || "", protocol: "http:", host: "d", href: "" },
    history: { pushState: (_s, _t, url) => book.pushed.push(url) },
    localStorage: { getItem: () => "stored-name" }, navigator: {},
    WebSocket: function (url) { const s = { url: String(url), readyState: 1, sent: [], send: (x) => s.sent.push(JSON.parse(x)), close() { s.closed = true; } }; book.sockets.push(s); return s; },
    requestAnimationFrame: (fn) => book.frames.push(fn), setTimeout: (fn, ms) => { book.timers.push({ fn, ms }); return book.timers.length; }, clearTimeout() {}, setInterval() {},
    addEventListener() {}, confirm: () => true, prompt: () => "Renamed",
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  for (const f of scripts) vm.runInContext(fs.readFileSync(path.join(__dirname, f), "utf8"), ctx, { filename: f });
  const settle = async () => { for (let i = 0; i < 12; i++) await new Promise((r) => setImmediate(r)); book.frames.splice(0).forEach((fn) => fn()); };
  await settle();
  const $ = (id) => doc.getElementById(id);
  const stream = () => book.sockets.filter((s) => s.url.includes("/ws?")).pop();
  const frame = async (f) => { stream().onmessage({ data: JSON.stringify(f) }); await settle(); };
  return { doc, book, $, settle, stream, frame, lists, posts: () => book.calls.filter((c) => c.method === "POST") };
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
  p.$("input").value = "What landed since yesterday?";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.deepEqual(p.posts().map((c) => [c.url, c.body]), [
    ["/v1/sessions", { cwd: "/p/app", workspaceId: "ws1", model: "", kind: "lead" }],
    ["/v1/sessions/lead1/prompt", { text: "What landed since yesterday?", sender: "Pat", images: [] }],
  ]);
  assert.equal(p.book.pushed.pop(), "/threads.html?s=lead1");
  assert.match(p.stream().url, /\/v1\/sessions\/lead1\/ws\?boot=&after=0$/);
  assert.equal(p.$("input").value, "", "sent, so the composer is empty");
});

test("a message that cannot be sent stays in the composer and says why", async () => {
  const p = await boot({ sessions: [lead], search: "?s=lead1", routes: [[/^POST \/v1\/sessions\/lead1\/prompt/, () => ({ fail: 409, error: "this session has ended; start a new one" })]] });
  p.$("input").value = "still there?";
  p.$("composer").onsubmit({ preventDefault() {} });
  await p.settle();
  assert.equal(p.$("input").value, "still there?");
  assert.equal(p.$("error-bar").textContent, "Not sent: this session has ended; start a new one");
  assert.equal(p.$("error-bar").classList.contains("hidden"), false);
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
  assert.equal(p.$("model-bar-text").textContent, "Haiku 5.5 applies on the next restart. This session keeps running on Opus 5.5 until then.");
  p.$("later").click();
  await p.settle();
  assert.equal(p.$("model-bar").classList.contains("hidden"), true);
  assert.equal(p.$("restart-later").textContent, "Restart to apply Haiku 5.5");
  assert.equal(p.$("restart-later").classList.contains("hidden"), false);
  p.$("restart-later").click();
  await p.settle();
  assert.equal(p.posts().pop().url, "/v1/sessions/lead1/restart");
  assert.equal(p.$("restarted-bar").classList.contains("hidden"), true, "not restarted until the new runner says so");
  await p.frame({ t: "snapshot", boot: "b2", seq: 0, data: { messages: [], info: { model: "claude-haiku-5-5" } } });
  assert.equal(p.$("restarted-bar").classList.contains("hidden"), false);
  assert.equal(p.$("restarted-bar").textContent, "Restarted on Haiku 5.5. The session picked up where it was.");
});

test("the firehose moves a session's dot and has the lists read again", async () => {
  const p = await boot({ sessions: [lead] });
  const feed = p.book.sockets[0];
  feed.onmessage({ data: JSON.stringify({ t: "attention", workspace: "ws1", pane: "pane1", state: "needs_input", since: 9 }) });
  await p.settle();
  assert.equal(p.$("tree").querySelector(".dot").className, "dot waiting");
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

test("a finished turn on screen is reported as seen, once", async () => {
  const p = await boot({ sessions: [{ ...lead, attention: "done", attentionSince: 77 }], search: "?s=lead1" });
  await p.settle();
  const acted = () => p.posts().filter((c) => c.url === "/v1/sessions/lead1/acted").length;
  assert.equal(acted(), 1);
  p.doc.handlers.visibilitychange();
  await p.settle();
  assert.equal(acted(), 1, "the same claim is not reported twice");
});
