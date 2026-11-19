import { test } from "node:test";
import assert from "node:assert/strict";
import { readOnlyVerdict } from "./readonly.mjs";

// A filesystem where every path is its own real path.
const fs = { real: (p) => p, link: () => null };
const verdict = (name, input, o, cwd = "/r/api") => readOnlyVerdict(name, input, o, cwd, fs);

test("a session that is no thread member is left alone", () => {
  assert.equal(verdict("Write", { file_path: "/r/api/a.go" }, {}), null);
  assert.equal(verdict("Bash", { command: "rm -rf /" }, {}), null);
  assert.equal(verdict("EnterWorktree", {}, {}), null);
});

test("a read-only member reads freely and is moved by its first change", () => {
  const o = { readOnly: true };
  for (const [name, input] of [["Read", { file_path: "/r/api/a.go" }], ["Grep", { pattern: "x" }], ["Bash", { command: "git log -3 | head" }], ["mcp__claude-peers__send_message", { message: "hi" }], ["AskUserQuestion", {}]]) {
    assert.equal(verdict(name, input, o), null, name);
  }
  for (const [name, input] of [
    ["Write", { file_path: "/r/api/a.go" }], ["Edit", { file_path: "a.go" }], ["MultiEdit", { file_path: "a.go" }], ["NotebookEdit", { notebook_path: "n.ipynb" }],
    ["Bash", { command: "npm test" }], ["Bash", { command: "echo x > a.go" }], ["Bash", {}], ["Monitor", { command: "tail -f log | tee x" }], ["PowerShell", { command: "Set-Content a b" }],
  ]) assert.deepEqual(verdict(name, input, o), { move: true }, name + " " + JSON.stringify(input));
});

test("Claude Code's own worktrees are off for a member, read only or not", () => {
  for (const o of [{ readOnly: true }, { keepOut: "/r/api" }]) {
    assert.match(verdict("EnterWorktree", {}, o).deny, /made by ccmux/);
    assert.match(verdict("ExitWorktree", {}, o).deny, /made by ccmux/);
    assert.match(verdict("Agent", { isolation: "worktree", prompt: "x" }, o).deny, /made by ccmux/);
    assert.equal(verdict("Agent", { prompt: "x" }, o), null);
  }
});

test("a member in its worktree is kept out of the repo's own folder", () => {
  const o = { keepOut: "/r/api" };
  const wt = "/wt/api/thread-3";
  assert.equal(verdict("Write", { file_path: wt + "/src/a.go" }, o, wt), null);
  assert.equal(verdict("Edit", { file_path: "src/a.go" }, o, wt), null);
  assert.equal(verdict("Write", { file_path: "/r/api-docs/a.md" }, o, wt), null, "a folder that only starts the same is another folder");
  const refused = verdict("Edit", { file_path: "/r/api/src/a.go" }, o, wt);
  assert.match(refused.deny, /the repo's own folder/);
  assert.match(refused.deny, /\/wt\/api\/thread-3\/src\/a\.go/, "it names the file's copy in the worktree");
  assert.equal(verdict("Read", { file_path: "/r/api/src/a.go" }, o, wt), null);
});

test("a member in its worktree may read the repo's folder from the shell, not change it", () => {
  const o = { keepOut: "/r/api" };
  const wt = "/wt/api/thread-3";
  assert.equal(verdict("Bash", { command: "cat /r/api/go.mod" }, o, wt), null);
  assert.equal(verdict("Bash", { command: "npm test" }, o, wt), null);
  assert.equal(verdict("Bash", { command: "cp x /r/api-docs/x" }, o, wt), null);
  assert.match(verdict("Bash", { command: "cd /r/api && git commit -am x" }, o, wt).deny, /Run it in your worktree \(\/wt\/api\/thread-3\)/);
  assert.match(verdict("Bash", { command: "cp a.go /r/api/" }, o, wt).deny, /the repo's own folder/);
  assert.match(verdict("Bash", { command: "git -C /r/api checkout main" }, o, wt).deny, /the repo's own folder/);
});

test("a task's builder is kept out with its file tools only", () => {
  const o = { keepOutFiles: "/r/api" };
  const wt = "/wt/api/task-12";
  assert.match(verdict("Write", { file_path: "/r/api/src/a.go" }, o, wt).deny, /Its copy in your worktree is \/wt\/api\/task-12\/src\/a.go/);
  assert.match(verdict("Edit", { file_path: "/r/api/a.go" }, o, wt).deny, /the repo's own folder/);
  assert.equal(verdict("Write", { file_path: wt + "/src/a.go" }, o, wt), null);
  // What its brief tells it to do: copy the env files out of the main checkout.
  for (const command of ["cp /r/api/.env.local .", "cp /r/api/.env.local ./.env.local && ls", "cat /r/api/.env > .env", "ln -s /r/api/x y"]) {
    assert.equal(verdict("Bash", { command }, o, wt), null, command);
  }
  // The full fence refuses the same copy, which is why a builder has the other one.
  assert.match(verdict("Bash", { command: "cp /r/api/.env.local ." }, { keepOut: "/r/api" }, wt).deny, /names \/r\/api/);
  // Claude Code's own worktrees are off for it too, said without the read-only hint.
  const own = verdict("EnterWorktree", {}, o, wt).deny;
  assert.match(own, /made by ccmux, so/);
  assert.match(verdict("EnterWorktree", {}, { readOnly: true }).deny, /the first change you make moves you/);
});
