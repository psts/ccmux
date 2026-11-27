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
    [{ state: "asleep", stuck: true }, "stuck"],
    [{ state: "running", stuck: true }, "idle"], // woken again: the mark is the daemon's to clear
    [{ state: "done", attention: "running" }, "ended"],
    [{ state: "unknown", attention: "needs_input" }, "unknown"],
    [null, "asleep"],
  ];
  for (const [session, want] of rows) assert.equal(m.stateOf(session), want, JSON.stringify(session));
  assert.equal(m.stateLabel("waiting"), "Needs you");
  assert.equal(m.stateLabel("stuck"), "Could not start");
  assert.equal(m.stateLabel("something-new"), "something-new");
});

test("git state reads like the lens: branch, dirty count, origin, and the default branch when off it", () => {
  const g = m.gitView({ isGitRepo: true, branch: "dev", trackingBranch: "origin/dev", ahead: 1, behind: 0, defaultBranch: "main", aheadOfDefault: 4, modifiedFiles: [{ path: "a" }], untrackedFiles: [{ path: "b" }] });
  assert.deepEqual(g, { branch: "dev", dirty: 2, origin: "↑1", ship: 4, shipBehind: 0, shipRef: "main", vs: "vs main ↑4", track: "dev → origin/dev ↑1", clean: "2 changed files" });
  const vsOrigin = m.gitView({ isGitRepo: true, branch: "dev", trackingBranch: "origin/dev", defaultBranch: "main", defaultRef: "origin/main", aheadOfDefault: 74, behindDefault: 2 });
  assert.equal(vsOrigin.shipBehind, 2, "behind origin/main shows folded too");
  assert.deepEqual([vsOrigin.ship, vsOrigin.shipRef, vsOrigin.vs, vsOrigin.origin], [74, "origin/main", "vs origin/main ↑74 ↓2", ""], "the daemon's ref names what is counted against");
  const mainAhead = m.gitView({ isGitRepo: true, branch: "main", trackingBranch: "origin/main", ahead: 3, behind: 1, defaultBranch: "main", defaultRef: "origin/main" });
  assert.deepEqual([mainAhead.ship, mainAhead.origin, mainAhead.track], [3, "↓1", "main → origin/main ↑3 ↓1"], "on main, what is not on origin/main is the number to ship, not a second arrow");
  const mainOther = m.gitView({ isGitRepo: true, branch: "main", trackingBranch: "fork/main", ahead: 3, defaultBranch: "main", defaultRef: "origin/main" });
  assert.deepEqual([mainOther.ship, mainOther.origin], [0, "↑3"], "tracking something else is no ship");
  const onMain = m.gitView({ isGitRepo: true, branch: "main", trackingBranch: "origin/main", defaultBranch: "main" });
  assert.deepEqual([onMain.vs, onMain.origin, onMain.track, onMain.clean], ["", "", "main → origin/main up to date", "Clean"]);
  assert.equal(m.gitView({ isGitRepo: true, branch: "dev", defaultBranch: "main" }).vs, "vs main even");
  assert.equal(m.gitView({ isGitRepo: true, branch: "x" }).track, "x (no upstream)");
  assert.equal(m.gitView({ isGitRepo: false }), null);
  assert.deepEqual(m.syncOf({ isGitRepo: true, branch: "dev", trackingBranch: "origin/dev", ahead: 1, defaultBranch: "main", defaultRef: "origin/main", aheadOfDefault: 74, behindDefault: 2 }), { ahead: 74, behind: 2, ref: "origin/main" }, "off the trunk: against the trunk, not its upstream");
  assert.deepEqual(m.syncOf({ isGitRepo: true, branch: "main", trackingBranch: "origin/main", ahead: 3, behind: 1, defaultBranch: "main", defaultRef: "origin/main" }), { ahead: 3, behind: 1, ref: "origin/main" }, "on the trunk: its own ahead and behind");
  assert.deepEqual(m.syncOf({ isGitRepo: true, branch: "master", trackingBranch: "origin/master", behind: 4, defaultBranch: "master", defaultRef: "origin/master" }), { ahead: 0, behind: 4, ref: "origin/master" });
  assert.equal(m.syncOf({ isGitRepo: true, branch: "main", trackingBranch: "fork/main", ahead: 3, defaultBranch: "main", defaultRef: "origin/main" }), null, "a trunk that tracks another remote says nothing against origin/main");
  assert.equal(m.syncOf({ isGitRepo: true, branch: "main", trackingBranch: "origin/main", defaultBranch: "main", defaultRef: "origin/main" }), null, "level: nothing");
  assert.equal(m.syncOf({ isGitRepo: false }), null);
  assert.equal(m.syncOf(undefined), null);
  assert.equal(m.gitView(undefined), null);
  assert.equal(m.ab(0, 3), "↓3");
  assert.equal(m.ab(2, 1), "↑2 ↓1");
});

test("the sidebar is windows, then repos with their lead, then the threads filed under each", () => {
  const windows = [{ id: "w1", name: "ChartLabs", workspaceIds: ["x-post", "app", "radar", "api", "gone"] }, { id: "w2", name: "Empty", workspaceIds: [] }];
  const workspaces = [
    { id: "app", name: "app", repoPath: "/p/app", git: { isGitRepo: true, branch: "dev" } },
    { id: "api", name: "", repoPath: "/p/backend" },
    { id: "radar", name: "radar", agent: "radar", status: "live" },
    { id: "x-post", name: "X-post", agent: "x-post", status: "cold" },
    { id: "solo", name: "solo", repoPath: "/p/solo" },
    { id: "lone", name: "lone", agent: "lone" },
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
  assert.deepEqual(app.threads.map((t) => [t.session.id, t.title, t.state]), [["t1", "New thread", "waiting"], ["t2", "Drawings lost on exit", "running"]]);
  assert.deepEqual([api.name, api.lead, api.state, api.threads.length], ["backend", null, "asleep", 0]);
  assert.deepEqual(groups[0].repos.map((r) => r.name), ["app", "backend"], "repos by name, not the window's order");
  assert.deepEqual(groups[0].agents, [{ id: "radar", name: "radar", state: "live" }, { id: "x-post", name: "X-post", state: "asleep" }], "agents apart from the repos, by name, ignoring case");
  assert.deepEqual(groups[1].repos.map((r) => r.name), ["solo"], "an agent's own workspace is not a repo");
  assert.deepEqual(groups[1].agents.map((a) => a.name), ["lone"], "an agent in no window is under Other");
  assert.deepEqual(m.tree([], [{ id: "a", name: "a", agent: "a" }], []).map((g) => [g.name, g.repos.length, g.agents.length]), [["Other", 0, 1]], "a group of agents alone is listed");
  assert.deepEqual(m.tree(undefined, undefined, undefined), []);
});

test("a lead is titled after its repo, a thread after itself", () => {
  assert.equal(m.titleOf({ kind: "lead", title: "ignored" }, { name: "app" }), "app");
  assert.equal(m.titleOf({ title: "Trade CSV export" }, { name: "app" }), "Trade CSV export");
  assert.equal(m.titleOf({}, { name: "app" }), "New thread");
});

test("a repo's planner is titled Planner, and is a session of its repo, never its lead", () => {
  assert.equal(m.titleOf({ kind: "planner", title: "Planner" }, { name: "app" }), "Planner");
  assert.equal(m.titleOf({ kind: "planner" }, { name: "app" }), "Planner", "never New thread, and never its first prompt");
  const ws = { id: "ws1", name: "app", repoPath: "/p/app" };
  const [row] = m.tree([{ id: "w1", name: "ChartLabs", workspaceIds: ["ws1"] }], [ws], [
    { id: "lead1", workspaceId: "ws1", kind: "lead", state: "running", createdAt: 1 },
    { id: "plan1", workspaceId: "ws1", kind: "planner", title: "Planner", state: "running", createdAt: 2 },
  ])[0].repos;
  assert.equal(row.lead.id, "lead1");
  assert.deepEqual(row.threads.map((t) => [t.session.id, t.title]), [["plan1", "Planner"]]);
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

test("before a session says which models it offers, the picker names them without a version", () => {
  const v = m.modelView({ model: "" }, {});
  assert.deepEqual(v.options.map((o) => [o.value, o.label]), [["opus", "Opus (default)"], ["fable", "Fable"], ["sonnet", "Sonnet"], ["haiku", "Haiku"]]);
  assert.equal(m.modelView({ model: "sonnet" }, { models: [{ value: "sonnet", name: "Sonnet 6", model: "claude-sonnet-6" }] }).wantedName, "Sonnet 6", "once it says, its own names, with the version");
});

// The terminal lens's rule (flash.js): an agent finishes often and on its
// own, so its green lasts a minute from when it finished; a repo's lasts
// until someone looks. Orange always lasts until answered.
test("an agent's done stops pulsing a minute after it finished; a repo's and a waiting one's do not", () => {
  const home = { id: "h", name: "radar", agent: "radar", status: "cold" };
  const repo = { id: "r", name: "app", repoPath: "/p/app" };
  const now = Date.now();
  const agentAt = (since, attention = "done") => ({ id: "a1", workspaceId: "h", kind: "agent", state: "running", attention, attentionSince: since });
  const lead = { id: "l1", workspaceId: "r", kind: "lead", state: "running", attention: "done", attentionSince: now - 3600000 };
  const stateAt = (agent) => m.tree([{ id: "w", name: "W", workspaceIds: ["h", "r"] }], [home, repo], [agent, lead])[0];
  assert.equal(stateAt(agentAt(now - 30000)).agents[0].state, "done", "half a minute in: still green");
  assert.equal(stateAt(agentAt(now - 61000)).agents[0].state, "idle", "past a minute: quiet");
  assert.equal(stateAt(agentAt(now - 61000, "needs_input")).agents[0].state, "waiting", "orange lasts until answered");
  assert.equal(stateAt(agentAt(0)).agents[0].state, "done", "no time known: as before");
  assert.equal(stateAt(agentAt(now - 61000)).repos[0].state, "done", "a repo's green waits for a look, however long");
});

test("an agent that runs as a session carries it: its row is the session's state", () => {
  const home = { id: "h", name: "radar", agent: "radar", status: "cold" };
  const still = { id: "t", name: "x-post", agent: "x-post", status: "live", panes: [{ id: "p" }] };
  const sess = { id: "a1", workspaceId: "h", kind: "agent", name: "radar", state: "running", attention: "needs_input" };
  const ended = { id: "a0", workspaceId: "h", kind: "agent", state: "done" };
  const [g] = m.tree([{ id: "w", name: "W", workspaceIds: ["h", "t"] }], [home, still], [ended, sess]);
  assert.deepEqual(g.agents, [{ id: "h", name: "radar", state: "waiting", session: sess }, { id: "t", name: "x-post", state: "live" }]);
  assert.equal(g.repos.length, 0, "its session is filed under no repo");
  assert.equal(m.tree([], [home], [{ ...sess, state: "asleep" }])[0].agents[0].state, "asleep");
  assert.equal(m.tree([], [home], [ended])[0].agents[0].session, undefined, "an ended session is not its session");
  assert.equal(m.titleOf(sess, home), "radar", "its header is the agent's name");
  assert.equal(m.titleOf({ kind: "agent", name: "radar" }, null), "radar");
});
