// threadcommits_test.js: how the Commits tab reads the daemon's answers
// (threadcommits.js). The tab in the page, with its clicks, is driven in
// threads_test.js.
const test = require("node:test");
const assert = require("node:assert/strict");
const C = require("./threadcommits.js");

const now = 1_800_000_000_000;
const hour = 3600 * 1000;

test("the groups, with a count that holds the ones not listed", () => {
  const v = C.listView({ repo: true, groups: [
    { key: "unpushed", title: "Not pushed", commits: [{ sha: "a".repeat(40), short: "aaaaaaa", subject: "One", time: now - 2 * hour }], more: 0 },
    { key: "pushed", title: "Pushed, not shipped", commits: [], more: 0 },
    { key: "release", title: "Since the last release", commits: [{ sha: "b".repeat(40), subject: "", time: now - 10 * 1000 }], more: 4 },
  ] }, now);
  assert.deepEqual(v.groups.map((g) => [g.title, g.count, g.more, g.rows.map((r) => [r.short, r.subject, r.when])]), [
    ["Not pushed", 1, "", [["aaaaaaa", "One", "2 h ago"]]],
    ["Since the last release", 5, "and 4 more", [["bbbbbbb", "(no message)", "just now"]]],
  ], "an empty group is left out; a row with no short sha or no subject still reads");
});

test("the quiet lines", () => {
  assert.deepEqual(C.listView({ repo: false, groups: [] }, now), { quiet: "Not a git repo." });
  assert.deepEqual(C.listView({ repo: true, groups: [] }, now), { quiet: "Nothing waiting: every commit has shipped." });
  assert.deepEqual(C.listView({}, now), { quiet: "Nothing waiting: every commit has shipped." }, "an answer from a daemon without the route");
  assert.deepEqual(C.listView(null, now), { quiet: "Nothing waiting: every commit has shipped." });
});

test("a file's line over its diff", () => {
  assert.equal(C.fileLabel({ path: "a.go", added: 3, removed: 1 }), "a.go · +3 −1");
  assert.equal(C.fileLabel({ path: "new.md", from: "old.md", added: 0, removed: 0 }), "old.md → new.md · +0 −0");
  assert.equal(C.fileLabel({ path: "logo.png", binary: true }), "logo.png · binary");
  assert.equal(C.metaLine({ sha: "0123456789abcdef", author: "Pat", time: now - 3 * hour }, now), "0123456789 · Pat · 3 h ago");
});

// A page with no DOM: draw is only asked to follow the session here, so
// the box is a stub that takes the drawing and is never read.
function fakePage() {
  const calls = [];
  const page = { calls, call: async (method, path) => { calls.push(path); return { repo: true, groups: [] }; }, changed() {}, now: () => now };
  return page;
}

test("the list is read for a new session, at a turn's end and when the tab is shown, never at every draw", async () => {
  global.document = { createElement: () => ({ appendChild() {}, set textContent(_) {} }) };
  const page = fakePage();
  const tab = C.create(page);
  const box = { appendChild() {}, set textContent(_) {} };
  tab.draw(box, "", false);
  assert.deepEqual(page.calls, [], "no session, nothing to read");
  tab.draw(box, "s 1", false);
  tab.draw(box, "s 1", false);
  assert.deepEqual(page.calls, ["/v1/sessions/s%201/commits"], "once per session, its id escaped");
  tab.draw(box, "s 1", true);
  tab.draw(box, "s 1", true);
  assert.equal(page.calls.length, 1, "a running turn reads nothing");
  tab.draw(box, "s 1", false);
  assert.equal(page.calls.length, 2, "the turn ended");
  tab.shown();
  assert.equal(page.calls.length, 3, "the tab came on screen");
  tab.draw(box, "s2", false);
  assert.equal(page.calls.pop(), "/v1/sessions/s2/commits");
  delete global.document;
});
