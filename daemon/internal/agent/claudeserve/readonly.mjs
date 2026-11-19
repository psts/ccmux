// Read only, and keep out: the two fences of a thread member (a thread's
// lead or one of its helpers, internal/threads in the daemon).
//
// A member starts in its repo's own folder with --read-only: it reads
// there and nothing else, because a person or the repo's own session may be
// working in that folder. The first tool call that would change something
// (a file-writing tool, or a shell command that does not plainly read) is
// refused and answered with "move": the agent asks the daemon for the
// member's own worktree and is restarted in it (agent.mjs readOnlyHook).
//
// In its worktree it runs with --keep-out <the repo's own folder>: Claude
// still remembers that folder's paths from its reading, and a write to one
// of them would land in the folder it was moved out of. Such a write is
// refused with the path it should use instead.
//
// A queued task's builder (store/builders.go in the daemon) starts in its
// worktree and runs with --keep-out-files <the repo's own folder>: the same
// fence for the file-writing tools, and none for shell commands. Its brief
// tells it to copy the env files it needs out of that folder, and the full
// fence refuses every command that names the folder and is not a plain
// read, "cp <repo>/.env.local ." among them.
//
// All are judged in a PreToolUse hook, which runs before every permission
// rule and mode, so no saved "always" and no permission mode steps over
// them. Not covered: MCP tools, which the session's own servers bring.
// Pure but for the path lookups it shares with the walls.
import { isAbsolute, relative, resolve, sep } from "node:path";
import { WRITE_TOOLS, realish } from "./walls.mjs";
import { plainRead } from "./readcommands.mjs";

// SHELL_TOOLS run a command line, named by this input field.
const SHELL_TOOLS = { Bash: "command", Monitor: "command", PowerShell: "command" };

// OWN_WORKTREE are Claude Code's own ways into a worktree of its making,
// inside the repo's folder. A member's worktree is the daemon's to make.
const OWN_WORKTREE = new Set(["EnterWorktree", "ExitWorktree"]);

// ownWorktreeDeny is the refusal of a worktree of Claude Code's making. A
// member still reading is told how it gets the one ccmux makes.
function ownWorktreeDeny(o) {
  const how = o.readOnly ? " (the first change you make moves you into it)" : "";
  return `ccmux: this session's worktree is made by ccmux${how}, so Claude Code's own worktrees are off here.`;
}

function makesOwnWorktree(name, input) {
  return OWN_WORKTREE.has(name) || ((name === "Agent" || name === "Task") && !!input && input.isolation === "worktree");
}

function inside(path, root) {
  return path === root || path.startsWith(root.endsWith(sep) ? root : root + sep);
}

// keptOut judges a call of a member in its worktree against dir, the folder
// it must not write in: null when the call stays out of it. Shell commands
// are judged only when shellToo is set (--keep-out, not --keep-out-files).
function keptOut(name, input, dir, shellToo, cwd, fs) {
  const out = realish(resolve(dir), fs);
  const field = WRITE_TOOLS[name];
  if (field) {
    const raw = input && input[field];
    if (typeof raw !== "string" || !raw) return null;
    const target = realish(isAbsolute(raw) ? resolve(raw) : resolve(cwd, raw), fs);
    if (!inside(target, out)) return null;
    return { deny: `ccmux: ${raw} is in ${dir}, the repo's own folder, which this session does not change. Its copy in your worktree is ${resolve(cwd, relative(out, target))}: change that one.` };
  }
  const shell = shellToo && SHELL_TOOLS[name];
  const line = shell && input ? input[shell] : "";
  if (typeof line !== "string" || !mentions(line, dir) || plainRead(line)) return null;
  return { deny: `ccmux: this command names ${dir}, the repo's own folder, which this session does not change. Run it in your worktree (${cwd}) instead; reading there is fine.` };
}

// mentions reports whether a command line names dir or something in it.
function mentions(line, dir) {
  for (let at = line.indexOf(dir); at >= 0; at = line.indexOf(dir, at + 1)) {
    const after = line[at + dir.length];
    if (after === undefined || !/[A-Za-z0-9_.-]/.test(after)) return true;
  }
  return false;
}

// readOnlyVerdict decides one tool call of a thread member: null when the
// fences have no say, {move: true} when a read-only member must be moved
// into its worktree first, {deny: text} when the call is refused outright.
export function readOnlyVerdict(name, input, o, cwd, fs) {
  const dir = o.keepOut || o.keepOutFiles;
  if (!o.readOnly && !dir) return null;
  if (makesOwnWorktree(name, input)) return { deny: ownWorktreeDeny(o) };
  if (!o.readOnly) return keptOut(name, input, dir, !!o.keepOut, cwd, fs);
  if (WRITE_TOOLS[name]) return { move: true };
  const shell = SHELL_TOOLS[name];
  if (!shell) return null;
  return plainRead(input && input[shell]) ? null : { move: true };
}
