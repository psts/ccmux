// When a paste becomes an uploaded image, and where its path goes (paste.js).
// Run: node --test daemon/web/paste_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const P = require("./paste.js");

const png = (name = "image.png", size = 100) => ({ name, type: "image/png", size });
const clip = ({ files = [], items = [], text = "" } = {}) => ({ files, items, getData: (t) => (t === "text/plain" ? text : "") });

test("a screenshot, or a copied image file, is an image", () => {
  const shot = png();
  assert.strictEqual(P.imageFrom(clip({ files: [shot] })), shot);
  const copied = png("Screenshot 2026-11-02.png");
  assert.strictEqual(P.imageFrom(clip({ files: [copied], text: "Screenshot 2026-11-02.png" })), copied);
  const viaItems = png();
  assert.strictEqual(P.imageFrom(clip({ items: [{ kind: "file", getAsFile: () => viaItems }] })), viaItems);
});

test("text, even with a picture riding along, pastes as text", () => {
  assert.strictEqual(P.imageFrom(clip({ text: "hello" })), null);
  assert.strictEqual(P.imageFrom(clip({ files: [png()], text: "a cell from a sheet" })), null);
  assert.strictEqual(P.imageFrom(clip({ files: [{ name: "notes.pdf", type: "application/pdf", size: 9 }] })), null);
  const tiff = { name: "x.tiff", type: "image/tiff", size: 9 };
  assert.strictEqual(P.imageFrom(clip({ files: [tiff] })), tiff, "any picture counts; it is converted later");
  const shot = png();
  assert.strictEqual(P.imageFrom(clip({ files: [tiff, shot] })), shot, "a kind the daemon takes wins when a copy offers both");
});

// el builds a fake element inside the terminal, or inside a chat box.
function el(where) {
  const ta = {
    tagName: "TEXTAREA", value: "see ", selectionStart: 4, selectionEnd: 4, events: [],
    setRangeText(s, a, b) { this.value = this.value.slice(0, a) + s + this.value.slice(b); },
    dispatchEvent(ev) { this.events.push(ev.type); },
    closest: (sel) => (where === "chat" && sel.includes(".ac-compose") ? ta : null),
  };
  if (where === "terminal") ta.closest = (sel) => (sel === "#terminal" ? {} : null);
  if (where === "elsewhere") ta.closest = () => null;
  return ta;
}

// paste runs one paste event and reports what happened.
// `open` is the pane on screen; `connected` whether typing into it works.
// switchTo moves the window to another pane while the upload is in flight.
// toPng stands in for the browser's canvas: it "converts" a picture to a
// PNG it names, or fails like a browser that cannot read the picture.
async function paste({ where = "terminal", data, answer = { status: 201, body: { path: "/tmp/ccmux-paste/paste-ab.png" } },
  open = { ws: "ws-1", pane: "p1" }, connected = true, switchTo = null, canDraw = true, drawError = null }) {
  const target = el(where);
  const got = { typed: [], failed: [], kept: [], posted: [], converted: [], prevented: false, target };
  let current = open;
  const e = { target, clipboardData: data, preventDefault() { got.prevented = true; }, stopPropagation() {} };
  await P.onPaste(e, {
    currentPane: () => current,
    typeIntoPane: (to, t) => { if (connected) got.typed.push([to.pane, t]); return connected; },
    fail: (m, keep) => { got.failed.push(m); got.kept.push(Boolean(keep)); },
    toPng: async (img) => {
      got.converted.push(img.type);
      if (!canDraw) throw Object.assign(new Error("unreadable"), { unreadable: true });
      if (drawError) throw new Error(drawError);
      return { name: "converted.png", type: "image/png", size: img.size };
    },
    fetch: async (url, opts) => {
      got.posted.push({ url, opts });
      if (switchTo) current = switchTo.gone ? null : switchTo;
      return { status: answer.status, json: async () => answer.body };
    },
  });
  return got;
}

global.Event = function (type) { this.type = type; };

test("an image pasted into a terminal uploads and types its path", async () => {
  const shot = png();
  const r = await paste({ data: clip({ files: [shot] }) });
  assert.ok(r.prevented);
  assert.strictEqual(r.posted[0].url, "/v1/workspaces/ws-1/paste");
  assert.strictEqual(r.posted[0].opts.body, shot);
  assert.deepStrictEqual(r.typed, [["p1", "/tmp/ccmux-paste/paste-ab.png "]]);
});

test("a move during the upload keeps the path out of every box, and shows it", async () => {
  const moves = [{ ws: "ws-1", pane: "p2" }, { ws: "ws-2", pane: "p1" }, null]; // another pane, another session, the board
  for (const where of ["terminal", "chat"]) {
    for (const switchTo of moves) {
      const r = await paste({ where, data: clip({ files: [png()] }), switchTo: switchTo || { gone: true } });
      assert.deepStrictEqual(r.typed, [], `${where}: nothing typed after a move`);
      assert.strictEqual(r.target.value, "see ", `${where}: the chat box keeps its draft`);
      assert.match(r.failed[0], /left that pane.*paste-ab\.png/);
      assert.deepStrictEqual(r.kept, [true]);
    }
  }
});

test("staying put, the path goes in, or a dropped socket says so", async () => {
  const offline = await paste({ data: clip({ files: [png()] }), connected: false });
  assert.deepStrictEqual(offline.typed, []);
  assert.match(offline.failed[0], /not connected.*\/tmp\/ccmux-paste\/paste-ab\.png/, "the path is in the note, not lost");
  assert.deepStrictEqual(offline.kept, [true], "and that note stays until closed");
});

test("an image pasted into the chat box lands at the cursor", async () => {
  const r = await paste({ where: "chat", data: clip({ files: [png()] }) });
  assert.strictEqual(r.target.value, "see /tmp/ccmux-paste/paste-ab.png ");
  assert.deepStrictEqual(r.target.events, ["input"]);
  assert.deepStrictEqual(r.typed, []);
});

test("text, and pastes outside the terminal and chat, are left to the browser", async () => {
  for (const r of [await paste({ data: clip({ text: "ls -la" }) }), await paste({ where: "elsewhere", data: clip({ files: [png()] }) })]) {
    assert.strictEqual(r.prevented, false);
    assert.strictEqual(r.posted.length, 0);
    assert.deepStrictEqual(r.typed, []);
  }
});

test("a failed upload says so and types nothing", async () => {
  const refused = await paste({ data: clip({ files: [png()] }), answer: { status: 415, body: { error: "not an image" } } });
  assert.deepStrictEqual(refused.typed, []);
  assert.match(refused.failed[0], /not an image/);
  const huge = await paste({ data: clip({ files: [png("big.png", 11 << 20)] }) });
  assert.strictEqual(huge.posted.length, 0, "an image over the cap is not sent");
  assert.match(huge.failed[0], /10 MB/);
  const noSession = await paste({ data: clip({ files: [png()] }), open: null });
  assert.strictEqual(noSession.posted.length, 0);
  assert.strictEqual(noSession.failed.length, 1);
  assert.deepStrictEqual([...refused.kept, ...huge.kept, ...noSession.kept], [false, false, false], "notes with no path clear themselves");
});

// fakeDoc is just enough document for showNote: one body, found by id.
function fakeDoc() {
  const doc = { kids: [] };
  const node = (tag) => ({
    tag, kids: [], append(...k) { this.kids.push(...k); },
    remove() { doc.kids = doc.kids.filter((x) => x !== this); },
  });
  doc.createElement = node;
  doc.body = { appendChild: (n) => doc.kids.push(n) };
  doc.getElementById = (id) => doc.kids.find((n) => n.id === id) || null;
  return doc;
}

test("a paste note shows with no pane open, and only a note without a path clears itself", () => {
  const doc = fakeDoc();
  const timers = [];
  const later = (fn) => timers.push(fn);
  P.showNote(doc, "The image was not pasted: not an image", false, later);
  assert.strictEqual(doc.getElementById("paste-bar").kids[0].textContent, "The image was not pasted: not an image");
  timers.splice(0).forEach((fn) => fn());
  assert.strictEqual(doc.getElementById("paste-bar"), null, "cleared after its time");
  const kept = P.showNote(doc, "not typed: /tmp/p.png", true, later);
  assert.strictEqual(timers.length, 0, "a note with a path sets no timer");
  P.showNote(doc, "newer", false, later);
  assert.strictEqual(doc.kids.length, 1, "a newer note replaces the older one");
  assert.notStrictEqual(doc.getElementById("paste-bar"), kept);
});

test("a picture the daemon does not take is pasted as PNG, or named when it cannot be read", async () => {
  const shot = png();
  const asIs = await paste({ data: clip({ files: [shot] }) });
  assert.deepStrictEqual(asIs.converted, [], "a PNG is sent as it is");
  assert.strictEqual(asIs.posted[0].opts.body, shot);
  const heic = { name: "IMG_0001.HEIC", type: "image/heic", size: 2 << 20 };
  for (const canDraw of [true, false]) { // Safari can open HEIC, Chrome cannot: the same either way
    const sent = await paste({ data: clip({ files: [heic] }), canDraw });
    assert.deepStrictEqual(sent.converted, [], "a HEIC is never redrawn: as a PNG it would pass the cap");
    assert.strictEqual(sent.posted[0].opts.body, heic, "it goes as it is, for the host to convert");
    assert.deepStrictEqual(sent.typed, [["p1", "/tmp/ccmux-paste/paste-ab.png "]]);
  }
  const tiff = { name: "scan.tiff", type: "image/tiff", size: 9 };
  const drawn = await paste({ data: clip({ files: [tiff] }) });
  assert.deepStrictEqual(drawn.converted, ["image/tiff"], "another picture is redrawn");
  assert.strictEqual(drawn.posted[0].opts.body.type, "image/png");
  const bmp = { name: "x.bmp", type: "image/bmp", size: 9 };
  const unread = await paste({ data: clip({ files: [bmp] }), canDraw: false });
  assert.strictEqual(unread.prevented, true, "the paste is still taken, not dropped on xterm");
  assert.strictEqual(unread.posted.length, 0, "only HEIC is left to the host");
  assert.match(unread.failed[0], /cannot read BMP images/);
  const tooBig = await paste({ data: clip({ files: [{ name: "scan.tiff", type: "image/tiff", size: 9 }] }), drawError: "9000x9000 is too large to redraw" });
  assert.doesNotMatch(tooBig.failed[0], /cannot read/, "a picture that opened is not called unreadable");
  assert.match(tooBig.failed[0], /TIFF image could not be turned into a PNG \(9000x9000 is too large to redraw\)/);
});

test("a HEIC sent to a host too old to convert it says so", async () => {
  const heic = { name: "IMG_0001.HEIC", type: "image/heic", size: 2 << 20 };
  const old = await paste({ data: clip({ files: [heic] }), answer: { status: 415, body: { error: "not an image" } } });
  assert.match(old.failed[0], /not an image: .*may be too old to convert HEIC \(update its ccmux\), or the file is not a HEIC it can read/);
  const png = await paste({ data: clip({ files: [{ name: "a.png", type: "image/png", size: 9 }] }), answer: { status: 415, body: { error: "not an image" } } });
  assert.match(png.failed[0], /not pasted: not an image$/, "a non-HEIC refusal keeps the daemon's words");
  const now = await paste({ data: clip({ files: [heic] }), answer: { status: 415, body: { error: "the HEIC image could not be converted (x)" } } });
  assert.match(now.failed[0], /could not be converted \(x\)/, "a current host's own reason is kept");
});
