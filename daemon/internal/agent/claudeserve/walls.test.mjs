import { test } from "node:test";
import assert from "node:assert/strict";
import { wallVerdict, realish } from "./walls.mjs";

// A fake filesystem: these paths exist, /home/agent/link points into a repo.
const existing = new Map([
  ["/", "/"], ["/home", "/home"], ["/home/agent", "/home/agent"], ["/win", "/win"],
  ["/win/shared", "/win/shared"], ["/win/shared/radar", "/win/shared/radar"],
  ["/home/agent/link", "/repo/app"], ["/repo", "/repo"], ["/repo/app", "/repo/app"],
]);
// /home/agent/dangling is a link to a file not yet written in a repo.
const links = new Map([["/home/agent/dangling", "/repo/app/src/new.ts"]]);
const real = {
  real: (p) => {
    if (existing.has(p)) return existing.get(p);
    throw Object.assign(new Error("ENOENT"), { code: "ENOENT" });
  },
  link: (p) => (links.has(p) ? links.get(p) : null),
};
const o = { writeRoots: ["/home/agent", "/win/shared/radar"], writeInside: "allow" };

test("a write inside a root follows the edit gate", () => {
  assert.equal(wallVerdict("Write", { file_path: "/win/shared/radar/2026-10-25-x.md" }, o, "/home/agent", real), "allow");
  assert.equal(wallVerdict("Edit", { file_path: "memory/MEMORY.md" }, o, "/home/agent", real), "allow");
  assert.equal(wallVerdict("Edit", { file_path: "/home/agent/log.md" }, { ...o, writeInside: "ask" }, "/home/agent", real), "ask");
});

test("a write outside every root is refused with no card", () => {
  for (const [tool, input] of [
    ["Write", { file_path: "/repo/app/src/x.ts" }],
    ["Edit", { file_path: "/win/shared/seo-writer/brief.md" }],
    ["Edit", { file_path: "/win/shared/radar-old/x.md" }], // a prefix is not a folder
    ["NotebookEdit", { notebook_path: "/repo/app/n.ipynb" }],
    ["Write", { file_path: "../../repo/app/x" }],
    ["Write", { file_path: "/home/agent/link/escape.ts" }], // a symlink out of the home
    ["Write", { file_path: "/home/agent/dangling" }], // a link whose target a write would create
    ["Write", { file_path: "/home/agent/.claude/settings.local.json" }], // config that could lift the walls
    ["Edit", { file_path: "/win/shared/radar/.mcp.json" }],
    ["Write", {}],
  ]) {
    const v = wallVerdict(tool, input, o, "/home/agent", real);
    assert.ok(v && v.deny, `${tool} ${JSON.stringify(input)} = ${JSON.stringify(v)}`);
  }
});

test("no walls, or a tool that writes no file, is not the walls' call", () => {
  assert.equal(wallVerdict("Write", { file_path: "/repo/app/x" }, { writeRoots: [] }, "/home/agent", real), null);
  assert.equal(wallVerdict("Bash", { command: "rm -rf /repo" }, o, "/home/agent", real), null);
  assert.equal(wallVerdict("Read", { file_path: "/repo/app/x" }, o, "/home/agent", real), null);
});

test("realish resolves the deepest existing folder and keeps the rest", () => {
  assert.equal(realish("/home/agent/link/new/file.ts", real), "/repo/app/new/file.ts");
  assert.equal(realish("/home/agent/dangling", real), "/repo/app/src/new.ts");
  assert.equal(realish("/win/shared/radar/a.md", real), "/win/shared/radar/a.md");
});

test("the agent's own Claude memory store is a root; the rest of ~/.claude is not", () => {
  const mem = "/home/u/.claude/projects/-home-u--ccmux-a-radar-seo/memory";
  const withMem = { writeRoots: ["/home/agent", mem], writeInside: "allow" };
  assert.equal(wallVerdict("Write", { file_path: mem + "/MEMORY.md" }, withMem, "/home/agent", real), "allow");
  assert.equal(wallVerdict("Edit", { file_path: mem + "/topic.md" }, withMem, "/home/agent", real), "allow");
  for (const p of ["/home/u/.claude/settings.json", "/home/u/.claude/projects/-other-agent/memory/MEMORY.md", "/home/u/.claude/projects/-home-u--ccmux-a-radar-seo/x.jsonl"]) {
    assert.ok(wallVerdict("Write", { file_path: p }, withMem, "/home/agent", real).deny, p);
  }
});
