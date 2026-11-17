// node --test threadcaps_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const m = require("./threadcaps.js");

const now = Date.parse("2026-11-17T12:00:00Z");
const caps = [
  { id: "qa", kind: "plugin", source: "personal", description: "commit gates", cost: 1400, version: "1.9.0", commit: "abc1234def", pinned: true, updateAvailable: "1.9.2" },
  { id: "chrome-pool", kind: "plugin", source: "personal", cost: 900, version: "2.0.0", commit: "abc1234def" },
  { id: "glitchtip", kind: "skill", source: "this machine", cost: 200 },
  { id: "claude-ai-gmail", kind: "connector", connector: "claude.ai Gmail", source: "claude.ai" },
];

test("an entry's name, kind and cost read as on the mockup", () => {
  assert.equal(m.name(caps[3]), "Gmail");
  assert.equal(m.kindLabel("mcp"), "MCP");
  assert.equal(m.kindLabel("new-kind"), "new-kind");
  assert.equal(m.tokens(1400), "~1.4k tokens");
  assert.equal(m.tokens(0), "", "no estimate, nothing said");
  assert.equal(m.tokens(30), "<0.1k tokens", "small, but not nothing");
  assert.equal(m.brief("Read and triage issues. Use when asked."), "Read and triage issues");
  assert.equal(m.brief("says hello"), "says hello");
  const long = m.brief("Survey any codebase as a senior advisor and produce prioritized, self-contained implementation plans for other agents");
  assert.ok(long.length <= 90 && long.endsWith("…"), long);
  assert.equal(m.brief(undefined), "");
});

test("a new source's id comes from its address, in the letters the catalog takes", () => {
  assert.equal(m.sourceId("https://github.com/anthropics/claude-plugins-official.git"), "claude-plugins-official");
  assert.equal(m.sourceId("https://github.com/a/b/"), "b");
  assert.equal(m.sourceId("/home/me/my plugins"), "my-plugins");
  assert.equal(m.sourceId("https://x.invalid/.pins"), "pins", "a leading dot is dropped, so never the pins folder");
  assert.equal(m.sourceId(""), "");
  assert.equal(m.shortURL("https://github.com/Ryan-SAAS/saac-plugins"), "Ryan-SAAS/saac-plugins");
  assert.equal(m.shortURL("/srv/market"), "/srv/market");
});

test("a source says what is known: pinned entries behind, else up to date only when checked this week", () => {
  const fresh = { id: "personal", url: "https://github.com/me/claude-personal", commit: "4f2a91c0000", updated: "2026-11-17T08:00:00Z" };
  assert.deepEqual(m.sourceRow(fresh, caps, now), { id: "personal", name: "personal", where: "git · me/claude-personal · 4f2a91c · checked today", tag: { text: "1 update", tone: "warn" }, behind: 1 });
  const ok = m.sourceRow({ id: "official", url: "https://github.com/anthropics/x", updated: "2026-11-15T08:00:00Z" }, caps, now);
  assert.deepEqual([ok.where, ok.tag], ["git · anthropics/x · checked 2 days ago", { text: "Up to date", tone: "ok" }]);
  const old = m.sourceRow({ id: "saac", url: "https://github.com/a/b", updated: "2026-10-24T08:00:00Z" }, caps, now);
  assert.deepEqual([old.where, old.tag], ["git · a/b · checked 24 days ago", null], "weeks old: no claim either way");
  assert.equal(m.sourceRow({ id: "n", url: "/x" }, [], now).where, "git · /x · never checked");
});

test("an entry row: source, words, cost, version, pin, and where it is on", () => {
  const choices = { windows: { w1: ["qa", "chrome-pool"], w2: ["qa"] }, workspaces: { ws1: { glitchtip: "on" }, ws2: { qa: "off" } } };
  const names = { w1: "ChartLabs", w2: "Home" };
  assert.deepEqual(m.entryRow(caps[0], choices, names), {
    id: "qa", name: "qa", kind: "plugin", meta: "personal · commit gates · ~1.4k tokens · pinned at 1.9.0 · on in 2 groups",
    update: "update 1.9.0 → 1.9.2", pin: "Unpin",
  });
  const cp = m.entryRow(caps[1], choices, names);
  assert.equal(cp.meta, "personal · ~0.9k tokens · 2.0.0 · follows newest · on in ChartLabs");
  assert.deepEqual([cp.update, cp.pin], ["", "Pin"]);
  const gl = m.entryRow(caps[2], choices, names);
  assert.equal(gl.meta, "this machine · ~0.2k tokens · on in 1 repo");
  assert.equal(gl.pin, "", "not from a checkout: nothing to pin");
  assert.equal(m.entryRow(caps[3], choices, names).meta, "claude.ai · on nowhere");
});

test("a repo's rows: its own choice and what the group gives", () => {
  const data = { window: "w1", group: ["qa", "glitchtip"], choices: { "chrome-pool": "on", glitchtip: "off", deleted: "on" }, effective: ["chrome-pool", "deleted", "qa"] };
  const rows = m.repoRows(caps, data, "ChartLabs");
  assert.deepEqual(rows.map((r) => [r.id, r.state, r.note]), [
    ["qa", "group", "On from ChartLabs"],
    ["chrome-pool", "on", "Off in ChartLabs, on here"],
    ["glitchtip", "off", "On in ChartLabs, off here"],
    ["claude-ai-gmail", "group", "Off from ChartLabs"],
    ["deleted", "on", "Gone from the list, sessions skip it"],
  ]);
  assert.equal(m.repoNote("group", false, ""), "Off, this repo is in no group");
  assert.equal(m.repoNote("on", true, "G"), "On in G and here");
  assert.equal(m.cost(caps, data.effective), 2300, "what is on, by the catalog's estimates");
});

test("a group's rows are on or off", () => {
  const rows = m.groupRows(caps, { on: ["qa", "old"] });
  assert.deepEqual(rows.map((r) => [r.id, r.state]), [["qa", "on"], ["chrome-pool", "off"], ["glitchtip", "off"], ["claude-ai-gmail", "off"], ["old", "on"]]);
  assert.equal(rows[4].note, "Gone from the list, sessions skip it");
});

test("the lines under a panel: what sessions load, and who runs on the old list", () => {
  assert.equal(m.footLine(3000, "This repo's"), "This repo's sessions load about 3.0k tokens of these. Changes apply when a session next starts.");
  assert.equal(m.footLine(0, "This group's"), "This group's sessions load none of these. Changes apply when a session next starts.");
  const sessions = [{ id: "a", workspaceId: "ws1", loadoutStale: true }, { id: "b", workspaceId: "ws1" }, { id: "c", workspaceId: "ws2", loadoutStale: true }];
  assert.deepEqual(m.stale(sessions, ["ws1"]).map((s) => s.id), ["a"]);
  assert.equal(m.staleLine(1), "1 running session uses the old list.");
  assert.equal(m.staleLine(2), "2 running sessions use the old list.");
  assert.equal(m.staleLine(0), "");
});

test("what an Update answered, in one line", () => {
  assert.equal(m.updateNote({ source: { id: "personal" }, changed: [{}, {}], added: ["x"], behind: [{}] }), "personal: 2 changed, 1 new, 1 pinned behind.");
  assert.equal(m.updateNote({ source: { id: "p" }, changed: [], added: [] }), "p: nothing changed.");
});
