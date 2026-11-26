// node --test threadnotes_test.js
//
// A repo's Notes: notes.js, the decisions both pages share (the old lens's
// Scratchpad and the thread page's pop-up), and threadnotesctl.js, the
// pop-up itself, on a small fake DOM and a fake daemon.
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const N = require("./notes.js");

const tick = async () => { for (let i = 0; i < 8; i++) await new Promise((r) => setImmediate(r)); };

// daemonOf is a fake daemon: pads by workspace id, every request kept.
function daemonOf(pads) {
  const d = { pads, calls: [], down: false, hold: null };
  d.get = async (id) => {
    d.calls.push(["GET", id]);
    if (d.hold) await d.hold;
    if (d.down) throw new Error("unreachable");
    return { content: d.pads[id] || "", updatedAt: Date.UTC(2026, 10, 26, 13, 2), updatedBy: "pat" };
  };
  d.put = async (id, text, unloading) => {
    d.calls.push(["PUT", id, text, unloading]);
    if (d.down) throw new Error("unreachable");
    d.pads[id] = text;
    return { content: text, updatedAt: Date.UTC(2026, 10, 26, 14, 2), updatedBy: "pat" };
  };
  d.puts = () => d.calls.filter((c) => c[0] === "PUT");
  return d;
}

// coreOf runs notes.js over a plain text box, a status line, the given
// storage (an object) and timers fired by hand.
function coreOf(daemon, kept = {}, open = true) {
  const ta = { value: "", readOnly: false, selectionStart: 0, selectionEnd: 0, setSelectionRange(a, b) { this.selectionStart = a; this.selectionEnd = b; } };
  const line = { text: "", bad: false };
  const timers = [];
  const storage = { getItem: (k) => (k in kept ? kept[k] : null), setItem: (k, v) => { kept[k] = v; }, removeItem: (k) => { delete kept[k]; } };
  const notes = N.create({
    area: () => ta, status: (text, bad) => Object.assign(line, { text, bad: !!bad }), get: daemon.get, put: daemon.put,
    storage: () => storage, later: (fn) => { timers.push(fn); return timers.length; }, cancel: () => {}, open: () => open,
    focused: () => true, when: () => "14:02",
  });
  const fire = async () => { timers.splice(0).forEach((fn) => fn()); await tick(); };
  return { notes, ta, line, kept, fire };
}

test("freshly read notes show alone, put a draft back, or keep both under a marker", () => {
  assert.deepEqual(N.opened("a", null), { text: "a", line: null, drop: false, save: false });
  assert.deepEqual(N.opened("a", { text: "a", base: "x" }), { text: "a", line: null, drop: true, save: false }, "a draft the daemon holds is dropped");
  assert.equal(N.opened("a", { text: "a, more", base: "a" }).text, "a, more");
  const both = N.opened("theirs", { text: "mine", base: "old", at: 1 }, () => "then");
  assert.equal(both.text, "theirs\n\n---- not saved from this browser, then ----\nmine");
  assert.equal(both.save, true);
  assert.match(both.line, /saved elsewhere/);
});

test("only notes reach the box: a list or a page from the daemon is refused", () => {
  assert.throws(() => N.padOf([{ id: "ws1" }]), /not notes/, "the workspace list a wrong route answers with");
  assert.throws(() => N.padOf({}), /not notes/);
  assert.deepEqual(N.padOf({ content: "" }), { content: "" }, "empty notes are notes");
});

test("a page going away asks to outlive it only under 60 KB", () => {
  assert.equal(N.keepalive(true, "x".repeat(100)), true);
  assert.equal(N.keepalive(true, "x".repeat(70000)), false);
  assert.equal(N.keepalive(false, "x"), false);
});

test("the status line says the time alone today and the date before that", () => {
  const now = new Date(2026, 10, 26, 15, 0).getTime();
  assert.doesNotMatch(N.shortWhen(new Date(2026, 10, 26, 14, 2).getTime(), now), /2026/);
  assert.match(N.shortWhen(new Date(2026, 10, 20, 14, 2).getTime(), now), /2026/);
  assert.equal(N.savedLine({ updatedAt: 5, updatedBy: "pat" }, () => "14:02"), "Saved 14:02 by pat");
  assert.equal(N.savedLine({ updatedAt: 0 }), "", "never saved: nothing to say");
});

test("load reads the notes into a writable box, and typing saves after a pause", async () => {
  const daemon = daemonOf({ ws1: "first" });
  const c = coreOf(daemon);
  assert.equal(await c.notes.load("ws1"), true);
  assert.equal(c.ta.value, "first");
  assert.equal(c.ta.readOnly, false);
  assert.equal(c.line.text, "Saved 14:02 by pat");
  c.ta.value = "first, more";
  c.notes.typed();
  assert.equal(c.line.text, "Not saved yet");
  assert.ok(c.kept["ccmux-scratch-draft:ws1"], "the draft is kept until the daemon has it");
  await c.fire();
  assert.equal(daemon.pads.ws1, "first, more");
  assert.equal(c.kept["ccmux-scratch-draft:ws1"], undefined, "and dropped once it does");
  await c.fire();
  assert.equal(daemon.puts().length, 1, "nothing new typed, nothing sent");
});

test("a draft this browser kept comes back on the next open and is saved", async () => {
  const daemon = daemonOf({ ws1: "notes" });
  const kept = { "ccmux-scratch-draft:ws1": JSON.stringify({ text: "notes, typed while away", base: "notes", at: 1 }) };
  const c = coreOf(daemon, kept);
  await c.notes.load("ws1");
  assert.equal(c.ta.value, "notes, typed while away");
  assert.match(c.line.text, /Restored/);
  await c.fire();
  assert.equal(daemon.pads.ws1, "notes, typed while away");
  assert.deepEqual(Object.keys(kept), [], "saved, so the draft is gone");
});

test("a draft typed on notes someone has saved over goes below theirs, once", async () => {
  const daemon = daemonOf({ ws1: "theirs" });
  const kept = { "ccmux-scratch-draft:ws1": JSON.stringify({ text: "mine", base: "old", at: 1 }) };
  const c = coreOf(daemon, kept);
  await c.notes.load("ws1");
  assert.equal(c.ta.value, "theirs\n\n---- not saved from this browser, 14:02 ----\nmine");
  await c.fire();
  assert.equal(daemon.pads.ws1, c.ta.value);
  const again = coreOf(daemon, kept);
  await again.notes.load("ws1");
  assert.equal(again.ta.value, daemon.pads.ws1, "not added a second time");
});

test("a save made elsewhere is taken only when nothing unsaved is typed here, keeping the caret", async () => {
  const daemon = daemonOf({ ws1: "one" });
  const c = coreOf(daemon);
  await c.notes.load("ws1");
  c.ta.setSelectionRange(1, 2);
  daemon.pads.ws1 = "one, by dasha";
  await c.notes.remote("ws1");
  assert.equal(c.ta.value, "one, by dasha");
  assert.deepEqual([c.ta.selectionStart, c.ta.selectionEnd], [1, 2]);
  c.ta.value = "mine, unsaved";
  daemon.pads.ws1 = "dasha again";
  const gets = daemon.calls.length;
  await c.notes.remote("ws1");
  assert.equal(c.ta.value, "mine, unsaved");
  assert.equal(daemon.calls.length, gets, "not even read");
  await c.notes.remote("ws2");
  assert.equal(daemon.calls.length, gets, "another repo's save is not these notes'");
  const shut = coreOf(daemon, {}, false);
  await shut.notes.load("ws1");
  await shut.notes.remote("ws1");
  assert.equal(daemon.calls.length, gets + 1, "notes not on screen are not read again");
});

test("a failed read leaves the box read-only and empty, and nothing typed is saved over the notes", async () => {
  const daemon = daemonOf({ ws1: "precious" });
  daemon.down = true;
  const c = coreOf(daemon);
  assert.equal(await c.notes.load("ws1"), false);
  assert.equal(c.ta.readOnly, true);
  assert.equal(c.ta.value, "");
  assert.equal(c.line.bad, true);
  c.ta.value = "typed anyway";
  c.notes.typed();
  daemon.down = false;
  await c.fire();
  c.notes.flush();
  await tick();
  assert.equal(daemon.puts().length, 0);
  assert.equal(daemon.pads.ws1, "precious");
});

test("a read that lands after another one started is not shown", async () => {
  const daemon = daemonOf({ ws1: "one", ws2: "two" });
  const c = coreOf(daemon);
  let release;
  daemon.hold = new Promise((r) => { release = r; });
  const first = c.notes.load("ws1");
  const second = c.notes.load("ws2");
  daemon.hold = null;
  release();
  assert.equal(await first, false);
  assert.equal(await second, true);
  assert.equal(c.ta.value, "two");
  assert.equal(c.notes.id(), "ws2");
});

// --- the pop-up (threadnotesctl.js) on a fake DOM ---

class El {
  constructor(tag) { Object.assign(this, { tagName: tag.toUpperCase(), children: [], className: "", attrs: {}, on: {}, value: "", text: "", scrollTop: 0, selectionStart: 0, selectionEnd: 0 }); }
  get classList() {
    const set = () => new Set(this.className.split(/\s+/).filter(Boolean));
    const put = (s) => { this.className = [...s].join(" "); };
    return { contains: (c) => set().has(c), add: (c) => put(set().add(c)), remove: (c) => { const s = set(); s.delete(c); put(s); }, toggle: (c, on) => { const s = set(); if (on) s.add(c); else s.delete(c); put(s); } };
  }
  get textContent() { return this.text + this.children.map((c) => c.textContent).join(""); }
  set textContent(v) { this.text = String(v); this.children = []; }
  get scrollHeight() { return 40 * this.value.split("\n").length; }
  append(...kids) { this.children.push(...kids); }
  setAttribute(k, v) { this.attrs[k] = v; }
  addEventListener(type, fn) { this.on[type] = fn; }
  focus() { doc.activeElement = this; }
  setSelectionRange(a, b) { this.selectionStart = a; this.selectionEnd = b; }
  find(cls) { for (const c of this.children) { if (c.classList.contains(cls)) return c; const f = c.find(cls); if (f) return f; } return null; }
}
let doc;

// popup boots threadnotesctl.js over an overlay, a sheet and the fake daemon.
function popup(pads, repo = { id: "ws1", name: "app" }) {
  doc = { activeElement: null, createElement: (t) => new El(t) };
  const overlay = new El("div"), sheet = new El("div");
  overlay.className = "overlay hidden";
  const calls = [], winOn = {}, timers = [];
  const answer = (id) => ({ content: pads[id] || "", updatedAt: 1, updatedBy: "pat" });
  const ctx = { document: doc, console, window: {} };
  ctx.window.ccmuxNotes = N;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, "threadnotesctl.js"), "utf8"), ctx, { filename: "threadnotesctl.js" });
  const page = { repo };
  const notes = ctx.window.ccmuxThreadNotesCtl.create({
    call: async (method, url) => { calls.push([method, url]); return answer(/workspaces\/([^/]+)\//.exec(url)[1]); },
    fetch: async (url, init) => {
      calls.push([init.method, url, JSON.parse(init.body).content, init.keepalive]);
      pads[/workspaces\/([^/]+)\//.exec(url)[1]] = JSON.parse(init.body).content;
      return { ok: true, status: 200, json: async () => ({ content: JSON.parse(init.body).content, updatedAt: 2, updatedBy: "pat" }) };
    },
    user: () => "Pat", repo: () => page.repo, overlay: () => overlay, sheet: () => sheet, storage: () => null,
    later: (fn) => { timers.push(fn); return timers.length; }, cancel: () => {}, listen: (type, fn) => { winOn[type] = fn; },
  });
  const area = () => sheet.find("notes-text");
  const puts = () => calls.filter((c) => c[0] === "PUT");
  return { notes, overlay, sheet, calls, puts, winOn, area, page, shown: () => !overlay.classList.contains("hidden") };
}

test("the pop-up opens on the repo's notes, scrolled to the end with the caret there", async () => {
  const p = popup({ ws1: "one\ntwo\nthree" });
  await p.notes.open();
  assert.equal(p.shown(), true);
  assert.deepEqual(p.calls[0], ["GET", "/v1/workspaces/ws1/scratchpad?user=Pat"]);
  assert.equal(p.area().value, "one\ntwo\nthree");
  assert.equal(p.area().scrollTop, p.area().scrollHeight, "scrolled all the way down");
  assert.deepEqual([p.area().selectionStart, p.area().selectionEnd], [13, 13], "caret at the end");
  assert.equal(doc.activeElement, p.area());
  assert.equal(p.sheet.find("notes-sub").textContent, "app · shared with everyone");
  assert.equal(p.sheet.find("notes-status").textContent.startsWith("Saved "), true);
});

test("nothing picked, nothing opens", async () => {
  const p = popup({}, null);
  await p.notes.open();
  assert.equal(p.shown(), false);
  assert.equal(p.calls.length, 0);
});

test("Esc, ×, and a click on the dim area close the pop-up and save first", async () => {
  const p = popup({ ws1: "one" });
  const typeAndClose = async (text, close) => {
    await p.notes.open();
    p.area().value = text;
    p.area().on.input();
    close();
    await tick();
    assert.equal(p.shown(), false, text);
    assert.deepEqual(p.puts().pop(), ["PUT", "/v1/workspaces/ws1/scratchpad?user=Pat", text, false], text);
  };
  await typeAndClose("by escape", () => assert.equal(p.notes.escape(), true));
  assert.equal(p.notes.escape(), false, "a closed pop-up leaves Esc to the page");
  await typeAndClose("by the x", () => p.sheet.find("sheet-close").onclick());
  await typeAndClose("by the dim area", () => { p.overlay.onpointerdown({ target: p.overlay }); p.overlay.onclick({ target: p.overlay }); });
});

test("a drag that selects text and lets go on the dim area does not close it", async () => {
  const p = popup({ ws1: "one" });
  await p.notes.open();
  p.overlay.onpointerdown({ target: p.area() });
  p.overlay.onclick({ target: p.overlay });
  assert.equal(p.shown(), true);
  p.overlay.onpointerdown({ target: p.sheet });
  p.overlay.onclick({ target: p.sheet });
  assert.equal(p.shown(), true, "a click inside the sheet never closes");
});

test("unsaved typing on the same repo stays when it opens again; a page going away saves with keepalive", async () => {
  const pads = { ws1: "one" };
  const p = popup(pads);
  await p.notes.open();
  p.area().value = "one, unsaved";
  p.overlay.classList.add("hidden"); // shut without the save, as a failed one would leave it
  await p.notes.open();
  assert.equal(p.calls.filter((c) => c[0] === "GET").length, 1, "not read over");
  assert.equal(p.area().value, "one, unsaved");
  p.winOn.pagehide();
  await tick();
  assert.deepEqual(p.puts().pop(), ["PUT", "/v1/workspaces/ws1/scratchpad?user=Pat", "one, unsaved", true]);
});

test("another repo's notes replace the box at once, before they are read", async () => {
  const pads = { ws1: "one", ws2: "two" };
  const p = popup(pads);
  await p.notes.open();
  p.notes.close();
  p.page.repo = { id: "ws2", name: "web" };
  const opening = p.notes.open();
  assert.equal(p.area().value, "", "ws1's text never shows as ws2's");
  await opening;
  assert.equal(p.area().value, "two");
  assert.equal(p.sheet.find("notes-sub").textContent, "web · shared with everyone");
});
