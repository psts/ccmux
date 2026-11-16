// node --test threadmodel_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const m = require("./threadmodel.js");

test("where a session is, as one word", () => {
  const rows = [
    [{ state: "running", attention: "running" }, "running"],
    [{ state: "running", attention: "needs_input" }, "waiting"],
    [{ state: "running", attention: "done" }, "done"],
    [{ state: "running", attention: "idle" }, "idle"],
    [{ state: "running" }, "idle"],
    [{ state: "asleep", attention: "done" }, "asleep"],
    [{ state: "done", attention: "running" }, "ended"],
    [{ state: "unknown", attention: "needs_input" }, "unknown"],
    [null, "asleep"],
  ];
  for (const [session, want] of rows) assert.equal(m.stateOf(session), want, JSON.stringify(session));
  assert.equal(m.stateLabel("waiting"), "Needs you");
  assert.equal(m.stateLabel("something-new"), "something-new");
});

test("git state reads like the lens: branch, dirty count, origin, and the default branch when off it", () => {
  const g = m.gitView({ isGitRepo: true, branch: "dev", trackingBranch: "origin/dev", ahead: 1, behind: 0, defaultBranch: "main", aheadOfDefault: 4, modifiedFiles: [{ path: "a" }], untrackedFiles: [{ path: "b" }] });
  assert.deepEqual(g, { branch: "dev", dirty: 2, origin: "↑1", vs: "vs main ↑4", track: "dev → origin/dev ↑1", clean: "2 changed files" });
  const onMain = m.gitView({ isGitRepo: true, branch: "main", trackingBranch: "origin/main", defaultBranch: "main" });
  assert.deepEqual([onMain.vs, onMain.origin, onMain.track, onMain.clean], ["", "", "main → origin/main up to date", "Clean"]);
  assert.equal(m.gitView({ isGitRepo: true, branch: "dev", defaultBranch: "main" }).vs, "vs main even");
  assert.equal(m.gitView({ isGitRepo: true, branch: "x" }).track, "x (no upstream)");
  assert.equal(m.gitView({ isGitRepo: false }), null);
  assert.equal(m.gitView(undefined), null);
  assert.equal(m.ab(0, 3), "↓3");
  assert.equal(m.ab(2, 1), "↑2 ↓1");
});

test("the sidebar is windows, then repos with their lead, then the threads filed under each", () => {
  const windows = [{ id: "w1", name: "ChartLabs", workspaceIds: ["app", "api", "gone"] }, { id: "w2", name: "Empty", workspaceIds: [] }];
  const workspaces = [
    { id: "app", name: "app", repoPath: "/p/app", git: { isGitRepo: true, branch: "dev" } },
    { id: "api", name: "", repoPath: "/p/backend" },
    { id: "radar", name: "radar", agent: "radar" },
    { id: "solo", name: "solo", repoPath: "/p/solo" },
  ];
  const sessions = [
    { id: "t2", workspaceId: "app", title: "Drawings lost on exit", createdAt: 30, state: "running", attention: "running" },
    { id: "lead", workspaceId: "app", kind: "lead", createdAt: 10, state: "asleep" },
    { id: "t1", workspaceId: "app", title: "", createdAt: 20, state: "running", attention: "needs_input" },
    { id: "old", workspaceId: "app", title: "ended", createdAt: 5, state: "done" },
    { id: "elsewhere", workspaceId: "nope", createdAt: 1, state: "running" },
  ];
  const groups = m.tree(windows, workspaces, sessions);
  assert.deepEqual(groups.map((g) => g.name), ["ChartLabs", "Other"], "a window with no repo is not listed");
  const [app, api] = groups[0].repos;
  assert.equal(app.lead.id, "lead");
  assert.equal(app.state, "asleep");
  assert.equal(app.git.branch, "dev");
  assert.deepEqual(app.threads.map((t) => [t.session.id, t.title, t.state]), [["t1", "New thread", "waiting"], ["t2", "Drawings lost on exit", "running"]]);
  assert.deepEqual([api.name, api.lead, api.state, api.threads.length, api.git], ["backend", null, "asleep", 0, null]);
  assert.deepEqual(groups[1].repos.map((r) => r.name), ["solo"], "an agent's own workspace is not a repo");
  assert.deepEqual(m.tree(undefined, undefined, undefined), []);
});

test("a lead is titled after its repo, a thread after itself", () => {
  assert.equal(m.titleOf({ kind: "lead", title: "ignored" }, { name: "app" }), "app");
  assert.equal(m.titleOf({ title: "Trade CSV export" }, { name: "app" }), "Trade CSV export");
  assert.equal(m.titleOf({}, { name: "app" }), "New thread");
});

const offered = [
  { value: "default", name: "Default (recommended)", model: "claude-opus-5-5" }, { value: "opus", name: "Opus 5.5", model: "claude-opus-5-5" },
  { value: "claude-fable-5-2[1m]", name: "Fable 5.2", model: "claude-fable-5-2[1m]" }, { value: "haiku", name: "Haiku 5.5", model: "claude-haiku-5-5" },
];

test("the picker offers what the session offers, and marks the default", () => {
  const v = m.modelView({ model: "" }, { models: offered, model: "claude-opus-5-5" });
  assert.deepEqual(v.options.map((o) => o.label), ["Opus 5.5 (default)", "Fable 5.2", "Haiku 5.5"]);
  assert.deepEqual([v.wanted, v.runningName, v.pending], ["opus", "Opus 5.5", false]);
});

test("of everything a login can run, the picker offers the four current models, and a session's own", () => {
  const many = [
    { value: "default", name: "Default (recommended)", model: "claude-opus-5-5" }, { value: "opus", name: "Opus 5.5", model: "claude-opus-5-5" },
    { value: "claude-fable-5-2[1m]", name: "Fable 5.2", model: "claude-fable-5-2[1m]" }, { value: "sonnet", name: "Sonnet 5.5", model: "claude-sonnet-5-5" },
    { value: "haiku", name: "Haiku 5.5", model: "claude-haiku-5-5" }, { value: "claude-opus-4-8", name: "Opus 4.8", model: "claude-opus-4-8" },
    { value: "claude-sonnet-5", name: "Sonnet 5", model: "claude-sonnet-5" },
  ];
  assert.deepEqual(m.modelView({ model: "" }, { models: many }).options.map((o) => o.label), ["Opus 5.5 (default)", "Fable 5.2", "Sonnet 5.5", "Haiku 5.5"]);
  const old = m.modelView({ model: "claude-opus-4-8" }, { models: many, model: "claude-opus-4-8" });
  assert.deepEqual(old.options.map((o) => o.label), ["Opus 5.5 (default)", "Fable 5.2", "Sonnet 5.5", "Haiku 5.5", "Opus 4.8"]);
  assert.deepEqual([old.wanted, old.runningName, old.pending], ["claude-opus-4-8", "Opus 4.8", false]);
  const unknown = [{ value: "a", name: "Model A", model: "vendor-a" }, { value: "b", name: "Model B", model: "vendor-b" }];
  assert.deepEqual(m.modelView({ model: "b" }, { models: unknown }).options.map((o) => o.value), ["a", "b"], "a login with none of them keeps its whole list");
});

test("a model picked and not yet running is pending, by name", () => {
  const v = m.modelView({ model: "haiku" }, { models: offered, model: "claude-opus-5-5" });
  assert.deepEqual([v.wanted, v.wantedName, v.runningName, v.pending], ["haiku", "Haiku 5.5", "Opus 5.5", true]);
  const same = m.modelView({ model: "claude-fable-5-2[1m]" }, { models: offered, model: "claude-fable-5-2" });
  assert.equal(same.pending, false, "one model under two spellings is not a change");
});

test("before a session's first turn nothing is known to be running, so nothing is pending", () => {
  const v = m.modelView({ model: "sonnet" }, {});
  assert.deepEqual(v.options.map((o) => o.value), ["opus", "fable", "sonnet", "haiku"]);
  assert.deepEqual([v.wanted, v.runningName, v.pending], ["sonnet", "", false]);
  assert.equal(m.modelView(null, null).wanted, "opus");
});

test("a model the list does not hold still shows what is running", () => {
  const v = m.modelView({ model: "haiku" }, { models: offered, model: "claude-mystery-9" });
  assert.deepEqual([v.runningName, v.pending], ["claude-mystery-9", true]);
});

test("how long ago, in the page's words", () => {
  const now = 10_000_000;
  assert.equal(m.ago(now - 20_000, now), "just now");
  assert.equal(m.ago(now - 125_000, now), "2 min ago");
  assert.equal(m.ago(now - 7_200_000, now), "2 h ago");
  assert.equal(m.ago(now - 3 * 86_400_000, now), "3 d ago");
  assert.equal(m.ago(0, now), "just now");
});
