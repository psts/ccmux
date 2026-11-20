// node --test threadgroups_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const G = require("./threadgroups.js");

const windows = [
  { id: "w2", name: "Trading", workspaceIds: ["ws3"] },
  { id: "w1", name: "ChartLabs", workspaceIds: ["ws1", "ws2"] },
  { id: "w3", name: "agents only", workspaceIds: ["a1"] },
];

test("a repo moves to every other group by name, or out of its group", () => {
  assert.deepEqual(G.moveTargets(windows, "ws1"), [
    { label: "agents only", group: "agents only" }, { label: "Trading", group: "Trading" }, { label: "No group (Other)", group: "" },
  ]);
  assert.deepEqual(G.moveTargets(windows, "loose").map((t) => t.label), ["agents only", "ChartLabs", "Trading"], "a repo in no group has no 'No group'");
  assert.deepEqual(G.moveTargets([], "loose"), []);
  assert.equal(G.windowOf(windows, "ws3").name, "Trading");
  assert.equal(G.windowOf(windows, "nowhere"), null);
});

test("a new group's name is trimmed, never empty or Other, and joins a group that has it in another case", () => {
  assert.deepEqual(G.groupName("  Ops  ", windows), { name: "Ops" });
  assert.match(G.groupName("   ", windows).error, /needs a name/);
  assert.match(G.groupName("", windows).error, /needs a name/);
  assert.match(G.groupName(null, windows).error, /needs a name/);
  assert.match(G.groupName("other", windows).error, /no group/, "an empty name would take the repo out of its group, and Other is the no-group list");
  assert.deepEqual(G.groupName("trading", windows), { name: "Trading", existing: true });
});

test("the question before a move says where, that sessions restart, and what stays behind", () => {
  const q = G.moveQuestion({ name: "app" }, "ChartLabs", "Trading", { tasks: 2, threads: 1 });
  assert.match(q, /^Move app from ChartLabs to Trading\?/);
  assert.match(q, /running sessions restart to pick up Trading's sessions/);
  assert.match(q, /2 unfinished tasks stay in ChartLabs's queue/);
  assert.match(q, /1 open thread can no longer reach helpers left in ChartLabs/);
  const plain = G.moveQuestion({ name: "app" }, "", "Ops");
  assert.match(plain, /from no group to Ops/);
  assert.doesNotMatch(plain, /task|thread/);
  assert.match(G.moveQuestion({ name: "app" }, "Ops", ""), /to no group\?[\s\S]*their new sessions/);
  assert.match(G.moveQuestion({ name: "app" }, "Ops", "X", { tasks: 1 }), /1 unfinished task stays/);
});

test("the + dialog offers no group, every group, and a new one, and shows the one it was opened for", () => {
  const c = G.groupChoices(windows, "chartlabs");
  assert.equal(c.value, "ChartLabs");
  assert.deepEqual(c.options.map((o) => o.label), ["No group", "agents only", "ChartLabs", "Trading", "New group…"]);
  assert.equal(c.options[c.options.length - 1].value, G.NEW);
  assert.equal(G.groupChoices(windows, "gone").value, "");
  assert.equal(G.groupChoices(windows).value, "");
});

test("a folder from the list makes a plain repo session, in its group when one is picked", () => {
  const p = { name: "app", path: "/p/app" };
  assert.deepEqual(G.createBody(p, "Ops"), { name: "app", repoPath: "/p/app", createdBy: "web", startupCommand: "", group: "Ops" });
  assert.equal("group" in G.createBody(p, ""), false);
});
