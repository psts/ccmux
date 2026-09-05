# Agent specification: what to decide when you create an agent

Status: draft v1, 2026-09-05. Companion to `agents-plan.md`. Read this before
creating a new base agent, and again before adding one to a project.

An agent is a **base** (a folder of plain files, shared by every project) plus
zero or more **instances** (the base added to one project). The base says what
the agent is. The instance says what it is for here.

## 1. Identity

| Decide | Rule | Where it lands |
|---|---|---|
| Name | `[a-z0-9-]`, 3 to 24 chars, a role not a person ("x-poster", "kb-writer"). It is the peer name on the bus, the folder name, and what humans type. Unique across bases. | `agent.json.name`, folder name |
| Icon | One emoji or glyph. Shows in both lenses and in `list_peers`. | `agent.json.icon` |
| Description | One sentence, under 120 chars, states what it does and when to call it. This is what other agents read to decide whether to contact it. Write it for them. | `agent.json.description`, `set_summary` on start |
| Version | Semver in the base manifest. Bumped on every base change. Instances record what they started with. | `.claude-plugin/plugin.json.version` |

## 2. Role and boundaries (AGENTS.md)

Keep the base `AGENTS.md` under 150 lines. Sections, in this order:

1. **Role.** Two or three sentences. What it does, who it serves, what it never does.
2. **Inputs it expects.** The shapes of requests it will get over the bus and from humans. Example messages.
3. **Outputs it produces.** Files, posts, replies, delegations. Where they go.
4. **How it works.** The procedure, as numbered steps. Anything longer than ten lines becomes a skill.
5. **Quality bar.** How it checks its own work before reporting done.
6. **Bus manners.** Reply to `from_id`. Acknowledge delegations with `update_task`. Report completion once. Ask before destructive actions and name what will be destroyed.
7. **Memory protocol.** Read `memory/MEMORY.md` at start. Write one line per durable learning. Append one line per task to `log.md`. Never write to the base folder.
8. **Refusals.** What it declines and how it says so.

Do not put project facts in the base. Do not put procedures in the base body;
put them in skills and reference them.

## 3. Skills

- One skill per procedure the agent runs more than once: "post a thread",
  "review a draft against the style guide".
- Format: `skills/<name>/SKILL.md` with `name` and `description` frontmatter.
  Description says when to use it. Body is the procedure.
- Base skills are namespaced by the base name in Claude Code
  (`x-poster:post`) and bare in opencode. Never rely on a skill name being
  unique across agents.
- Project skills go in the instance `.claude/skills/`. Both Claude Code and
  opencode read that path.

## 4. Tools and MCP servers

- List every MCP server the agent needs in `mcp.json`, neutral shape:
  `{ "<name>": { "command": "...", "args": [...], "env": {...} } }`.
  Secrets come from env names, never literal values.
- `ccmux-peers` is always present; do not list it.
- Ask: does this tool let the agent do damage outside its role? If yes, the
  permission block (section 6) must gate it.

## 5. Harness, account, model

| Decide | Default | Notes |
|---|---|---|
| Harness | `opencode` | Claude Code for agents that need channels or hooks today; codex when the account is ChatGPT. |
| Account | the global route | Set only when the agent must pin one (a cheap model for a chatty agent, a local model for private data). Kinds are enforced by the pairing rule. |
| Model | harness default | Name it when quality or cost matters. Aliases resolve in the proxy. |
| Thinking / effort | harness default | Raise for judgment-heavy agents, lower for formatting agents. |

opencode instances always get `"snapshot": false` (cache prefix churn on new
files, see the spike) and the Meridian plugin when routed to a `meridian`
account.

## 6. Permissions

Write the permission block for the harness in `agent.json.permissions`, in the
neutral vocabulary `read | edit | bash | webfetch | external_directory` →
`allow | ask | deny`, plus a list of bash patterns. The adapter translates it.

Defaults: `read: allow`, `edit: allow` inside the instance folder and
`--add-dir` folders, `bash: ask`, `webfetch: ask`. An agent that posts to the
outside world (X, email, a CMS) gets `ask` on that tool until it has run
clean for a week, then `allow` with a bash pattern list.

## 7. Memory policy

Three layers. Decide what goes where.

| Layer | Owner | Lives | Example |
|---|---|---|---|
| Base knowledge | human | `knowledge/` in the base | style guide, API docs, tone examples |
| Project instructions | human, in git | instance `AGENTS.md`, `.claude/skills/` | which repo paths matter, project voice, who approves |
| Agent memory | the agent | instance `memory/` and `log.md` | "posts with images get 3x replies here", "last thread 2026-09-05" |

Rules: the base is read-only to instances. Memory is per instance by default.
Cross-project learning is human-curated into `knowledge/`. If an agent should
share memory across projects, say so in `agent.json.memory: "shared"` and
accept that project details will leak between them.

## 8. Lifecycle

| Decide | Default | When to change |
|---|---|---|
| Start mode | fresh conversation per start | `continue` for one-long-thread agents |
| Idle exit | 10 minutes with no open task | shorter for chatty cheap agents, longer for slow external APIs |
| Keep alive | off | on for agents that must answer inside a second |
| Concurrency | daemon cap shared by all agents | pin `maxConcurrent: 1` for agents with a rate-limited upstream |
| Autostart | off | on only with keep alive |

Implemented (2026-09-05): the daemon's lifecycle loop ticks every 30 s.
Busy/idle comes from the harness: Claude Code panes through their hooks,
opencode panes through the embedded ccmux plugin (`agents/.ccmux/
ccmux-opencode.ts`, listed in every instance's `opencode.jsonc`), which posts
to `POST /v1/panes/{id}/agent-signal` on loopback. An instance idle past
`idleExitMinutes` with no open peer delegation gets ctrl-d and drops to its
shell ("asleep"); a keep-alive instance found asleep is started again from
the current base, and a keep-alive instance whose base version moved exits
when idle so that restart picks the new base up. `-agents-max` (default 6)
caps running instances; asleep ones do not count.

## 9. Bus contract

- Discovery text = description. Other agents see: name, description, status.
- Contact by name starts it. Design the first message handling so that a
  cold start with a delegation task lands correctly: acknowledge, work, close.
- Replies always go to `from_id`.
- If the agent delegates, it uses `delegate`, not `send_message`, and it
  closes the loop on task updates.
- Permission relay: the agent answers `yes|no <id>` only for work it asked for.

Implemented (2026-09-05): `POST /v1/peers/agents` gives a session the bases
with their state in its own workspace; the shim appends an AGENTS ON THIS BUS
paragraph to the instructions and an Agents footer to `list_peers`.
`send_message(to_name=<agent>, spawn_if_missing=true)` from a pane starts or
wakes the agent in the SENDER's workspace and delivers the message once it
registers. opencode instances receive bus messages typed into their TUI
through the instance's server (channel pushes do not reach opencode); Claude
Code instances get them as channel messages.

## 10. Cost and safety caps

- `maxTurnsPerTask` (default 40) and `maxTokensPerTask` (default 400k) in
  `agent.json`. The daemon ends the session and reports when hit.
- External side effects (posting, sending, deleting) are listed in
  `agent.json.sideEffects` so the UI can show a warning badge.
- Anything that spends money or reaches the public needs a dry-run mode
  described in `AGENTS.md`.

## 11. Testing an agent before shipping the base

1. Start it in a scratch project with the composer. Give it the three
   example inputs from section 2 and compare against the quality bar.
2. Contact it from another session by name with `spawn_if_missing`. Confirm
   the cold-start path acknowledges and completes.
3. Delegate a task from another session. Confirm `update_task` states arrive
   and the loop closes without a second message.
4. Kill its harness mid-task. Confirm the next contact resumes sanely on
   memory and log, not on the transcript.
5. Read `memory/MEMORY.md` and `log.md` afterwards. Delete anything that is
   project detail in the base, or transcript noise in memory.
6. Bump the version and write one line in `CHANGELOG.md` in the base.

## 12. Adding a base to a project (instance checklist)

- Which window/group? The instance is reachable by name inside that group;
  other groups use `to_group`.
- Which repo folders does it need? `addDirs`.
- Which project instructions does it need on day one? Write them in the
  instance `AGENTS.md`. Keep the starter file's header.
- Any project-only skills? `.claude/skills/`.
- Memory location: default, or in-repo for team-shared memory.
- Account or model override for this project only.

## Files

### `agent.json`

```json
{
  "name": "x-poster",
  "icon": "𝕏",
  "description": "Writes and posts X threads about a project's releases and changes; ask it with a topic or a changelog link.",
  "harness": "opencode",
  "account": "",
  "model": "",
  "permissions": { "read": "allow", "edit": "allow", "bash": "ask", "webfetch": "allow",
                   "bashAllow": ["git log *", "git diff *"] },
  "memory": "per-instance",
  "start": "fresh",
  "idleExitMinutes": 10,
  "keepAlive": false,
  "maxTurnsPerTask": 40,
  "maxTokensPerTask": 400000,
  "sideEffects": ["posts publicly to X"],
  "addDirs": []
}
```

### Base folder

```
~/.ccmux/agents/x-poster/
├── .claude-plugin/plugin.json   { "name": "x-poster", "version": "1.0.0" }
├── agent.json
├── AGENTS.md
├── CLAUDE.md                    @AGENTS.md
├── CHANGELOG.md
├── skills/<name>/SKILL.md
├── mcp.json
└── knowledge/
```

### Instance folder

```
<repo>/.ccmux/agents/x-poster/
├── AGENTS.md          starter header + project instructions
├── CLAUDE.md          @AGENTS.md
├── .claude/skills/
├── memory/MEMORY.md
├── log.md
└── opencode.jsonc     GENERATED at every start from the base: instructions
                       path, permissions, MCP servers + peers bus, snapshot off.
                       The one daemon-owned file here; do not edit.
```

Implemented in `daemon/internal/agent` (2026-09-05): `Store` over
`~/.ccmux/agents` (`-agents-dir`), `Reject`, `Save` (bumps the patch version
when agent.json or AGENTS.md changed, appends CHANGELOG.md, never touches
skills/knowledge/mcp.json), `Delete`, `Bootstrap` (instance folder, once),
`WriteInstanceConfig`. API: `GET /v1/agents`, `PUT /v1/agents/{name}`
(per-name upsert, not list replace), `DELETE /v1/agents/{name}`.


## Lenses (2026-09-05)

Web: Settings → Agents (rows save on change, delete confirmed), the project
menu lists every base with its state here (● running opens it, ○ asleep
wakes it, + not yet added adds it), a sleeping agent pane shows a composer
over its history (Enter wakes it with the text as first message, with a ↻
note when the base has moved), and agent panes carry a ⚙ tab mark.

Mac: the same Agents tab and project-menu rows; the composer is a prompt box
opened from the menu row (the Mac has no bar over the pane), which is the
one deliberate difference between the lenses. Unbuilt on the Linux host; the
release tag job builds it.
