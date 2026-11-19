// node --test threadteam_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const Model = require("./threadmodel.js");
const t = require("./threadteam.js");

const workspaces = [
  { id: "app", name: "app", repoPath: "/p/app" }, { id: "api", name: "", repoPath: "/p/backend" }, { id: "adm", name: "admin", repoPath: "/p/admin" },
];
const lead = { id: "L", workspaceId: "app", threadId: 3, role: "lead", state: "running", attention: "idle", createdAt: 1, name: "app-thread-3", worktree: "/wt/app/thread-3", branch: "thread/3-export", goal: "Endpoint done, admin next." };
const backend = { id: "B", workspaceId: "api", threadId: 3, role: "helper", state: "running", attention: "done", createdAt: 3, name: "backend-thread-3", worktree: "/wt/backend/thread-3", branch: "thread/3-export" };
const admin = { id: "A", workspaceId: "adm", threadId: 3, role: "helper", state: "running", attention: "running", createdAt: 2, name: "admin-thread-3", readOnly: true };
const gone = { id: "G", workspaceId: "adm", threadId: 3, role: "helper", state: "done", createdAt: 1 };
const otherLead = { id: "O", workspaceId: "app", threadId: 4, role: "lead", state: "asleep", createdAt: 5 };
const plain = { id: "P", workspaceId: "app", state: "running", attention: "running", createdAt: 6 };
const all = [backend, plain, gone, admin, otherLead, lead];

test("a thread's members are its own sessions that have not ended, the lead first", () => {
  assert.deepEqual(t.membersOf(lead, all).map((s) => s.id), ["L", "A", "B"]);
  assert.deepEqual(t.membersOf(backend, all).map((s) => s.id), ["L", "A", "B"], "asked from a helper it is the same team");
  assert.deepEqual(t.membersOf(otherLead, all).map((s) => s.id), ["O"]);
  assert.deepEqual(t.membersOf(plain, all).map((s) => s.id), ["P"], "a session in no thread is on its own");
  assert.deepEqual(t.membersOf(null, all), []);
});

test("a thread needs you when any member does, and runs while any member runs", () => {
  assert.equal(t.teamState(lead, all), "running", "the lead is idle, admin is working");
  assert.equal(t.teamState(lead, [lead, { ...admin, attention: "needs_input" }, backend]), "waiting");
  assert.equal(t.teamState(lead, [lead, backend]), "idle", "a helper that finished does not make the thread run");
  assert.equal(t.teamState({ ...lead, state: "done" }, all), "ended", "an ended lead is ended, whatever its helpers did last");
  assert.equal(t.teamState(plain, all), "running");
});

test("the line under a thread says who it works with and where it stands", () => {
  assert.equal(t.subLine(lead, [lead, backend, { ...admin, attention: "needs_input" }], workspaces), "with admin, backend · needs you", "helpers in the order they were stood up");
  assert.equal(t.subLine(lead, all, workspaces), "with admin, backend · running");
  assert.equal(t.subLine(lead, [lead], workspaces), "idle", "a thread with no helpers says only where it stands");
  assert.equal(t.subLine(lead, [lead, backend], workspaces, { id: 3, state: "merging" }), "with backend · merging");
  assert.equal(t.subLine(lead, [lead], workspaces, { id: 3, state: "merged" }), "merged");
  assert.equal(t.subLine(lead, [{ ...lead, attention: "running" }], workspaces, { id: 3, state: "merged" }), "running", "new work after a merge shows as work");
  assert.equal(t.subLine(otherLead, all, workspaces), "asleep");
});

test("the sidebar's thread rows carry their team's state and line, and helpers are no rows of their own", () => {
  const tree = Model.tree([{ id: "w", name: "Shop", workspaceIds: ["app", "api", "adm"] }], workspaces, [lead, backend, { ...admin, attention: "needs_input" }, plain]);
  t.decorate(tree, [lead, backend, { ...admin, attention: "needs_input" }, plain], workspaces, [{ id: 3, state: "open" }]);
  const [app, api, adm] = ["app", "api", "adm"].map((id) => tree[0].repos.find((r) => r.id === id)); // the sidebar lists them by name
  assert.deepEqual(app.threads.map((r) => [r.session.id, r.state, r.sub]), [["L", "waiting", "with admin, backend · needs you"], ["P", "running", "running"]]);
  assert.deepEqual([api.threads, adm.threads], [[], []], "a helper is filed under its thread, not under its repo");
});

test("the Helpers tab lists each member with what it does and where it works", () => {
  assert.deepEqual(t.helperRows(lead, all, workspaces), [
    { id: "L", name: "app", bus: "app-thread-3", role: "lead", state: "idle", doing: "Endpoint done, admin next.", where: "worktree · thread/3-export" },
    { id: "A", name: "admin", bus: "admin-thread-3", role: "helper", state: "running", doing: "Working.", where: "repo folder · read only" },
    { id: "B", name: "backend", bus: "backend-thread-3", role: "helper", state: "done", doing: "Finished its turn.", where: "worktree · thread/3-export" },
  ]);
  assert.deepEqual(t.helperRows(plain, all, workspaces), [], "a session in no thread has no team");
  assert.deepEqual(t.helperRows(null, all, workspaces), []);
});

test("the header says who leads and who helps", () => {
  assert.equal(t.metaLine(lead, all, workspaces, "Shop"), "Shop · lead: app · helpers: admin, backend");
  assert.equal(t.metaLine(lead, [lead, backend], workspaces, "Shop"), "Shop · lead: app · helper: backend");
  assert.equal(t.metaLine(otherLead, all, workspaces, "Shop"), "Shop · lead: app");
  assert.equal(t.metaLine(backend, all, workspaces, "Shop"), "Shop · helper in backend · lead: app");
});

// A queued task's thread has no lead: the task queue leads it, and its
// members are builders, one per repo, each a thread row of its own repo.
test("a task's builder is named as one, with its teammates, and no lead is asked for", () => {
  const appB = { id: "TA", workspaceId: "app", threadId: 9, role: "builder", state: "running", attention: "running", createdAt: 1, name: "app-task-12", title: "Task #12: Export", worktree: "/wt/app/task-12", branch: "task/12-export" };
  const apiB = { id: "TB", workspaceId: "api", threadId: 9, role: "builder", state: "running", attention: "needs_input", createdAt: 2, name: "backend-task-12", worktree: "/wt/backend/task-12", branch: "task/12-export" };
  const both = [...all, apiB, appB];
  assert.equal(t.metaLine(appB, both, workspaces, "Shop"), "Shop · task builder in app · with: backend");
  assert.equal(t.metaLine(appB, [appB], workspaces, "Shop"), "Shop · task builder in app");
  assert.doesNotMatch(t.metaLine(apiB, both, workspaces, "Shop"), /lead/);
  assert.deepEqual(t.helperRows(appB, both, workspaces).map((r) => [r.name, r.role, r.bus, r.where]), [
    ["app", "builder", "app-task-12", "worktree · task/12-export"], ["backend", "builder", "backend-task-12", "worktree · task/12-export"]]);
  // Each builder is a row under its own repo: a helper is hidden there, a builder is not.
  const tree = t.decorate(Model.tree([], workspaces, both), both, workspaces, []);
  const rows = (name) => tree[0].repos.find((r) => r.name === name).threads.map((x) => x.session.id);
  assert.ok(rows("app").includes("TA"));
  assert.deepEqual(rows("backend"), ["TB"]);
  assert.equal(t.teamState(appB, both), "waiting", "a teammate's question shows on the task's rows");
});

test("changes are grouped by repo, with each member's branch and what it holds", () => {
  const detail = { thread: { id: 3 }, members: [
    { session: lead, repo: "app", status: "2 commits, all committed", changes: { commits: 2, files: [{ path: "src/a.js", added: 18, removed: 2 }, { path: "bin/logo.png" }] } },
    { session: admin, repo: "admin", status: "Reading only" },
  ] };
  assert.deepEqual(t.changeGroups(detail), [
    { repo: "app", role: "lead", branch: "thread/3-export", status: "2 commits, all committed", files: [{ path: "src/a.js", added: 18, removed: 2 }, { path: "bin/logo.png", added: 0, removed: 0 }] },
    { repo: "admin", role: "helper", branch: "no branch yet", status: "Reading only", files: [] },
  ]);
  assert.deepEqual(t.changeGroups(null), []);
});

test("a member whose repo session is not in the lists is named after its folder", () => {
  assert.equal(t.repoOf({ workspaceId: "nope", repo: "/p/payments" }, workspaces), "payments");
  assert.equal(t.repoOf({ workspaceId: "api" }, workspaces), "backend");
});

test("the merge bar says which step a running merge is in and for how long, and where a failed one's output is", () => {
  assert.equal(t.mergeLine(null, 0), null, "no merge job, no bar");
  assert.equal(t.mergeLine(undefined, 0), null);
  const running = { job: "merge-thread-3-ab12cd", unit: "ccmux-job@merge-thread-3-ab12cd.service", state: "running", doing: "backend: checks running", startedAt: 1000, attempts: 1 };
  assert.deepEqual(t.mergeLine(running, 73000), { running: true, stopping: false, text: "Merging · backend: checks running · 1m 12s" });
  assert.equal(t.mergeLine({ ...running, doing: "" }, 9000).text, "Merging · between steps · 8s", "between two steps it still says it runs");
  assert.equal(t.mergeLine({ ...running, attempts: 2 }, 3700000 + 1000).text, "Merging · backend: checks running · 1h 01m · started again after it was cut off (try 2)");
  assert.equal(t.mergeLine(running, 0).text, "Merging · backend: checks running · 0s", "a clock that runs behind the daemon's shows no negative time");
  assert.deepEqual(t.mergeLine({ ...running, state: "stopping" }, 5000), { running: true, stopping: true, text: "Stopping the merge…" });
  const failed = t.mergeLine({ ...running, state: "failed", checkLog: "/wt/backend-b4/thread-3.check.log" }, 5000);
  assert.deepEqual(failed, { running: false, stopping: false, text: "The last merge failed. The failed check's whole output is in /wt/backend-b4/thread-3.check.log" });
  assert.equal(t.mergeLine({ ...running, state: "failed" }, 5000).text, "The last merge failed.", "a failure that was no check names no file");
  assert.equal(t.mergeLine({ ...running, state: "lost" }, 5000).text, "The last merge did not finish. Its log: journalctl --user -u ccmux-job@merge-thread-3-ab12cd.service");
  assert.equal(t.mergeLine({ ...running, state: "stopped" }, 5000), null, "a merge someone stopped needs no bar");
  assert.deepEqual([0, 59000, 60000, 3599000, 3600000].map(t.elapsed), ["0s", "59s", "1m 00s", "59m 59s", "1h 00m"]);
});
