// What a key sends instead of xterm.js's own bytes (keys.js).
// Run: node --test daemon/web/keys_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const K = require("./keys.js");

const key = (k, mods = {}, type = "keydown") => ({ key: k, type, shiftKey: false, ctrlKey: false, altKey: false, metaKey: false, ...mods });

test("Shift+Enter sends Ctrl+J, Claude Code's new line", () => {
  assert.strictEqual(K.inputFor(key("Enter", { shiftKey: true })), "\n");
});

test("plain Enter, other modifiers and other keys stay with xterm.js", () => {
  assert.strictEqual(K.inputFor(key("Enter")), null);
  assert.strictEqual(K.inputFor(key("Enter", { shiftKey: true, ctrlKey: true })), null);
  assert.strictEqual(K.inputFor(key("Enter", { shiftKey: true, altKey: true })), null);
  assert.strictEqual(K.inputFor(key("Enter", { shiftKey: true, metaKey: true })), null);
  assert.strictEqual(K.inputFor(key("a", { shiftKey: true })), null);
});

test("Cmd+Left and Cmd+Right jump a word, as in the Mac app", () => {
  assert.strictEqual(K.inputFor(key("ArrowLeft", { metaKey: true })), "\x1bb");
  assert.strictEqual(K.inputFor(key("ArrowRight", { metaKey: true })), "\x1bf");
});

test("arrows without Cmd alone stay with xterm.js", () => {
  assert.strictEqual(K.inputFor(key("ArrowLeft")), null);
  assert.strictEqual(K.inputFor(key("ArrowLeft", { altKey: true })), null);
  assert.strictEqual(K.inputFor(key("ArrowLeft", { metaKey: true, shiftKey: true })), null);
  assert.strictEqual(K.inputFor(key("ArrowUp", { metaKey: true })), null);
});

test("an input method still composing keeps its Enter", () => {
  assert.strictEqual(K.inputFor(key("Enter", { shiftKey: true, isComposing: true })), null);
});

test("the terminal sends once per press and swallows the press's other events", () => {
  let handler = null;
  const sent = [];
  K.attach({ attachCustomKeyEventHandler: (fn) => { handler = fn; } }, (d) => sent.push(d));
  let prevented = 0;
  const ev = (type) => ({ ...key("Enter", { shiftKey: true }, type), preventDefault: () => { prevented++; } });
  assert.strictEqual(handler(ev("keydown")), false);
  assert.strictEqual(handler(ev("keypress")), false);
  assert.strictEqual(handler(ev("keyup")), false);
  assert.deepStrictEqual(sent, ["\n"]);
  assert.strictEqual(prevented, 1);
  assert.strictEqual(handler(key("Enter")), true);
  assert.deepStrictEqual(sent, ["\n"]);
});
