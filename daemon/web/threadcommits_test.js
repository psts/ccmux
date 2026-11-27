// threadcommits_test.js: how the Git tab reads the daemon's answers
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

test("a row carries its whole message and the line under it", () => {
  const v = C.listView({ repo: true, groups: [{ key: "pushed", title: "Pushed, not shipped", more: 0, commits: [
    { sha: "0123456789abcdef", short: "0123456", subject: "Fix it", body: "Why: it broke.", author: "Pat", time: now - 3 * hour },
    { sha: "f".repeat(40), subject: "No body", time: now - hour },
  ] }] }, now);
  assert.deepEqual(v.groups[0].rows.map((r) => [r.body, r.meta]), [["Why: it broke.", "0123456789 · Pat · 3 h ago"], ["", "ffffffffff · 1 h ago"]]);
});

test("the git line: branch, changes, upstream and what a ship would send, each with its words on hover", () => {
  const l = C.gitLine({ isGitRepo: true, branch: "dev", trackingBranch: "origin/dev", ahead: 2, behind: 1, defaultBranch: "main", defaultRef: "origin/main", aheadOfDefault: 74, behindDefault: 3, modifiedFiles: [{ path: "a" }] });
  assert.deepEqual(l, {
    branch: "dev", dirty: "●1", dirtyTip: "1 changed file", origin: "↑2 ↓1", originTip: "dev → origin/dev ↑2 ↓1",
    ship: "↑74 ↓3", shipTip: "74 commits not on origin/main yet: ready to ship. 3 commits on origin/main not in this branch",
  });
  const clean = C.gitLine({ isGitRepo: true, branch: "main", trackingBranch: "origin/main", defaultBranch: "main", defaultRef: "origin/main" });
  assert.deepEqual([clean.dirty, clean.origin, clean.ship, clean.originTip], ["", "", "", "main → origin/main up to date"], "a clean trunk shows its branch alone");
  assert.equal(C.gitLine(undefined), null, "a daemon that sent no status");
  assert.equal(C.gitLine({ isGitRepo: false }), null);
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
