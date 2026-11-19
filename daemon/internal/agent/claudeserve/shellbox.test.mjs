import { test } from "node:test";
import assert from "node:assert/strict";
import { shellBox } from "./shellbox.mjs";
import { parseArgs } from "./args.mjs";

test("a session with no walls gets no sandbox option at all", () => {
  assert.equal(shellBox([]), undefined);
  assert.equal(shellBox(undefined), undefined);
});

test("a walled session's commands are sandboxed into its walls' folders, with no way out", () => {
  const roots = ["/wt/task-1", "/repo/.git/objects"];
  const box = shellBox(roots);
  assert.equal(box.enabled, true);
  assert.equal(box.failIfUnavailable, true, "never a silent fall back to open commands");
  assert.equal(box.allowUnsandboxedCommands, false, "dangerouslyDisableSandbox must do nothing");
  assert.equal(box.autoAllowBashIfSandboxed, false, "the session's own permission mode still decides");
  assert.deepEqual(box.filesystem.allowWrite, roots);
  assert.notEqual(box.filesystem.allowWrite, roots, "its own list, not the caller's");
  assert.equal(box.network, undefined, "no host list: a builder's fetches and its loopback servers stay as they are");
});

test("the daemon's flag names one folder each time it is given", () => {
  const o = parseArgs(["--shell-box-write", "/wt/task-1", "--shell-box-write", "/home/u/.cache"]);
  assert.deepEqual(o.shellBoxWrite, ["/wt/task-1", "/home/u/.cache"]);
  assert.deepEqual(parseArgs([]).shellBoxWrite, []);
});
