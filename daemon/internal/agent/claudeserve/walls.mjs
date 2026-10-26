// Walls: an agent whose base turns them on (agent.json "walls": true)
// writes files only inside its write roots: its home, its Claude Code
// memory store (~/.claude/projects/<home>/memory, where its MEMORY.md is
// loaded from) and its own folder in the window's shared folder. The daemon passes them as --write-root and
// leaves the edit tools off the allow list. A PreToolUse hook (agent.mjs
// wallHook) refuses a write outside the roots before any saved allow rule
// is read, and canUseTool decides the rest, both here: inside a root it follows the base's edit
// gate (--write-inside allow skips the card), outside it is refused at once
// with no card to answer. Inside a root, Claude Code's own config (a
// .claude/ folder, .mcp.json) is refused too: a settings file there could
// allow every edit or switch the mode and step over the walls. Not
// covered: Bash (the bash gate's to judge) and MCP tools, which the base's
// own servers bring and the allow list lets through.
import { lstatSync, readlinkSync, realpathSync } from "node:fs";
import { dirname, isAbsolute, resolve, sep } from "node:path";

// WRITE_TOOLS maps each file-writing tool onto the input field naming the
// file it writes.
export const WRITE_TOOLS = { Edit: "file_path", Write: "file_path", MultiEdit: "file_path", NotebookEdit: "notebook_path" };

// realish is the real path of p, or of its deepest existing ancestor with
// the rest appended, so a file not yet written is judged by the folder it
// lands in. A symlink, even one whose target is missing (a write would
// create that target), is followed, so no link leads a write out of a root.
export function realish(p, fs = realFS, depth = 0) {
  const rest = [];
  let cur = p;
  for (;;) {
    try {
      return [fs.real(cur), ...rest.reverse()].join(sep).replace(/\/+/g, "/");
    } catch {
      const link = fs.link(cur);
      if (link !== null && depth < 32) {
        const target = resolve(dirname(cur), link);
        return realish([target, ...rest.reverse()].join(sep), fs, depth + 1);
      }
      const up = dirname(cur);
      if (up === cur) return p;
      rest.push(cur.slice(up.length).replace(/^\/+/, ""));
      cur = up;
    }
  }
}

// realFS is the filesystem realish reads: a path's real path, and a
// dangling symlink's target (null when cur is not a symlink).
const realFS = {
  real: realpathSync,
  link: (p) => {
    try {
      return lstatSync(p).isSymbolicLink() ? readlinkSync(p) : null;
    } catch {
      return null;
    }
  },
};

// claudeConfig is a path Claude Code reads its own settings, hooks or MCP
// servers from.
function claudeConfig(path) {
  const parts = path.split(sep);
  return parts.includes(".claude") || parts[parts.length - 1] === ".mcp.json";
}

function inside(path, root) {
  return path === root || path.startsWith(root.endsWith(sep) ? root : root + sep);
}

// wallVerdict decides a tool call against the walls: null when the walls
// have no say (no roots, or not a write tool), "allow" for a write inside
// a root when the edit gate allows it, "ask" inside a root otherwise, and
// a deny message for anything outside.
export function wallVerdict(name, input, o, cwd, fs = realFS) {
  const field = WRITE_TOOLS[name];
  if (!field || !o.writeRoots || !o.writeRoots.length) return null;
  const raw = input && input[field];
  if (typeof raw !== "string" || !raw) return { deny: `Walls: ${name} names no file, so it is refused.` };
  const target = realish(isAbsolute(raw) ? resolve(raw) : resolve(cwd, raw), fs);
  const roots = o.writeRoots.map((r) => realish(resolve(r), fs));
  const root = roots.find((r) => inside(target, r));
  if (root && claudeConfig(target.slice(root.length))) {
    return { deny: `Walls: ${raw} is Claude Code's own config, which a walled agent may not write.` };
  }
  if (root) return o.writeInside === "allow" ? "allow" : "ask";
  return {
    deny: `Walls: this agent writes only in its own folders (${o.writeRoots.join(", ")}); ${raw} is outside them. ` +
      "Data another agent owns changes through a request to that owner (delegate), never a write.",
  };
}

// wallSafe drops from a walled agent's "always" suggestions the ones that
// would lift the walls: a mode switch (acceptEdits approves every edit in
// the added folders without asking canUseTool, bypassPermissions all) and
// any rule naming a file-writing tool. A rule for anything else (a shell
// pattern) is the base's gates' business and stays.
export function wallSafe(suggestions) {
  return suggestions.filter((s) => {
    if (!s || s.type === "setMode") return false;
    const rules = Array.isArray(s.rules) ? s.rules : [];
    return !rules.some((r) => r && Object.hasOwn(WRITE_TOOLS, r.toolName));
  });
}
