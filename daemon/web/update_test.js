// When an open page moves onto a newer web lens (update.js).
// Run: node --test daemon/web/update_test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const fs = require("fs");
const path = require("path");
const U = require("./update.js");

const idle = { returning: true, busy: false, hidden: false };

test("the same build, or a build nobody knows, changes nothing", () => {
  assert.strictEqual(U.decide("aaa", "aaa", idle), "none");
  assert.strictEqual(U.decide(undefined, "bbb", idle), "none"); // a page from before builds
  assert.strictEqual(U.decide("aaa", "", idle), "none"); // the daemon did not answer
});

test("a page coming back into view with nothing going on reloads", () => {
  assert.strictEqual(U.decide("aaa", "bbb", idle), "reload");
});

test("a page in use gets the bar instead", () => {
  assert.strictEqual(U.decide("aaa", "bbb", { ...idle, busy: true }), "bar"); // unsent text, open dialog
  assert.strictEqual(U.decide("aaa", "bbb", { ...idle, returning: false }), "bar"); // on screen all along
});

test("a hidden page waits until it is shown", () => {
  assert.strictEqual(U.decide("aaa", "bbb", { ...idle, hidden: true }), "none");
});

// busy() finds unsent chat text and open dialogs by class name. Renamed on
// the other side, a page would reload over a half-written message.
test("the chat box and the dialogs still carry the names busy() looks for", () => {
  const src = (f) => fs.readFileSync(path.join(__dirname, f), "utf8");
  assert.match(src("update.js"), /"\.ac-compose textarea"/);
  assert.match(src("agentchat.js"), /<form class="ac-compose"><textarea/);
  assert.match(src("update.js"), /"\.modal:not\(\.hidden\)"/);
  const modals = src("index.html").match(/<div id="[^"]*-modal" class="[^"]*"/g) || [];
  assert.ok(modals.length >= 4, "index.html has its dialogs");
  for (const m of modals) assert.match(m, /class="modal[ "]/, m);
});

test("the logo names the daemon's version", () => {
  assert.strictEqual(U.versionLine({ version: "0.1.96", web: "aaa" }, "aaa"), "ccmux v0.1.96");
  assert.strictEqual(U.versionLine({ version: "dev", web: "aaa" }, "aaa"), "ccmux dev"); // a local build
});

test("and says when this page is behind it", () => {
  assert.strictEqual(U.versionLine({ version: "0.1.96", web: "bbb" }, "aaa"),
    "ccmux v0.1.96\nThis page is older than that: reload to update");
  // A build nobody knows is not "behind".
  assert.strictEqual(U.versionLine({ version: "0.1.96", web: "" }, "aaa"), "ccmux v0.1.96");
  assert.strictEqual(U.versionLine({ version: "0.1.96", web: "bbb" }, undefined), "ccmux v0.1.96");
});

test("a daemon that does not answer is said plainly", () => {
  assert.strictEqual(U.versionLine(null, "aaa"), "ccmux: version unknown, the daemon is not answering");
});
