# Standalone agents — plan and investigation

Status: **investigation, not agreed.** Written 2026-09-04. Nothing here is built.

> **Read the v2 section at the end first.** After discussion the model changed
> from "one standalone agent workspace" to "a base agent, instantiated per
> project". The v1 design below is kept for the findings it cites; its
> "Design (option A)" section is superseded by v2.

## The ask

Today every Claude session lives in a workspace, and a workspace is a repo folder.
Sessions in the same shared window form one peers-bus group and talk to each other.

We want **standalone agents**: a Claude that is not tied to any repo, has a fixed
role and its own knowledge (for example "writes knowledge-base articles"), is
created from a UI, and is reachable on the same peers bus from repo sessions.

## Short answer

Yes, it is possible, and most of it is already there. The bus routes on peer ids
and group strings only; nothing in delivery, tasks or the permission relay knows
what a repo is. Every repo assumption sits in two places: **how a peer gets its
name and group**, and **how a missing peer gets spawned**. Both are small.

The cheapest shape that fits every existing rule: **an agent is a workspace whose
folder is an agent home** (`~/.ccmux/agents/<name>/`) that the daemon writes from
an agent definition. That makes the file explorer, git badges, hooks, memory,
autoconfirm, revive and both sidebars work unchanged. What is new is the agent
definition (a settings object like harnesses), the launch line, a reach rule on
the bus so any group can call an agent by name, and an Agents tab in both lenses.

## What exists today (verified in source)

### A workspace is a folder

- `POST /v1/workspaces` refuses an empty `repoPath` (`internal/api/server.go:782-785`).
- `RepoPath` seeds the tmux session name, the default pane cwd, git status, the
  file API root and dev-port detection (`manager.go:193-196, 389-391`,
  `manager/gitstatus.go:51`, `api/files.go:102`).
- Git status on a folder that is not a repo is fine: `gitstatus.Full` returns
  `IsGitRepo=false` and both lenses already render a "Not a git repository" state
  (`manager/gitstatus.go:61-73`, `SidebarView.swift:948-956`).

### How a peer gets its identity

- Name: `CLAUDE_PEERS_NAME` if set, else basename of git root, else basename of
  cwd (`internal/peers/registry.go:61-68`). No `group` field exists on the wire
  (`registry.go:14-37`).
- Group ladder (`internal/peers/service.go:285-307`): pane's shared window →
  federation map → Mac local-pane map → `GroupOverride` → parent-folder fallback
  (`service.go:359-368`). `GroupOverride` is only ever set by a matched spawn
  (`spawn.go:150`).
- Reach rule (`internal/peers/reach.go:19-36`): same group, or `to_group` names
  the target's group, or a 2h reply grant. Name lookup is scoped to one group
  (`messages.go:87-125`).

### How a missing peer gets spawned

- `spawn_if_missing` first asks `LiveWorkspaceForRepo(group, name)`, which needs
  a live workspace in the same group whose folder basename equals the name
  (`manager.go:1058-1070`).
- Otherwise it guesses `dirname(sender repo)/<name>` and fails if that folder does
  not exist (`spawn.go:66-82`). No client sends `to_repo`.
- The spawned pane is ephemeral: its command is not persisted, so a revive brings
  back a bare shell (`manager.go:372-378`).

### How Claude is launched and what it knows

- The command is typed into a login zsh: `env -u TMUX claude
  --dangerously-load-development-channels server:claude-peers`
  (`harness/harness.go:94`). No system prompt, no `--resume`, no `--add-dir`.
- The only in-band instruction channel is the one-shot birth prompt after `--`
  (`spawn.go:25-30, 43-50`). Durable knowledge today comes only from the MCP
  server instructions (`cmd/ccmux-peers/tools.go:22-42`) and whatever CLAUDE.md
  sits in the cwd.
- The `claude` CLI on this host supports `--append-system-prompt[-file]`,
  `--add-dir`, `--resume <id>`, `--continue`, `--name`, `--model`,
  `--permission-mode` (checked with `claude --help` 2026-09-04).
- Revive replays the stored startup command with no `--resume`, so a daemon
  restart gives a fresh session with empty context (`manager.go:550-558`). The
  hooks already deliver Claude's `session_id` and the daemon stores it in
  `pane_sessions.live_ids` (`store/panesessions.go`).

### Settings objects, the pattern to copy

Harnesses are one JSON list under a settings key, a `Service` with `List`,
`Resolve`, `Reject` (validate) and `Apply` (whole-list replace), exposed as a
field of `GET/PUT /v1/settings` (`harness/harness.go:27-30, 156-264`,
`api/server.go:439-566`). Both lenses have an editor for it
(`daemon/web/app.js:1520-1623`, `DaemonSettingsView.swift:330-410`) and the Mac
lens gates each field on presence in the GET (`DaemonModels.swift:108-111`).

## Options

### A. Agent home folder as the workspace (recommended)

The daemon keeps an **agent definition** in settings and materializes a folder
`~/.ccmux/agents/<name>/` holding a generated `CLAUDE.md` plus a `knowledge/`
directory. Starting the agent creates (or revives) a workspace whose `repoPath`
is that folder, with a new `kind = "agent"` column so lenses can style it.

Pros
- Name derivation, hooks, autoconfirm, file explorer, git badges, layout,
  attach, hub federation and revive all work with **zero change**.
- Claude reads the folder's `CLAUDE.md` on its own, and its per-project memory
  is keyed on the cwd path, so the agent remembers across sessions for free.
- The user can drop reference files into `knowledge/` through the existing file
  explorer (Mac) or file API (web).
- Sidebar placement uses the shared-window model unchanged: an agent sits in a
  window like any workspace, so it is in that group.

Cons
- It is still "a folder". The sidebar shows a path unless `kind` hides it.
- One agent, one window. To be callable from every group we need one small bus
  rule (below), not a new model.

### B. New first-class entity (agents table, no workspace)

Pros: clean model, no folder on disk.
Cons: panes, tmux sessions, layout, attach, revive, archive guards, hub
aggregation and both sidebars all key on workspace id. This means a parallel
copy of most of the manager and both lenses. Weeks of work in execution terms,
high regression risk, for no user-visible gain over A.

### C. Pane-less external process (headless `claude -p` loop)

Pros: no terminal at all.
Cons: pane-less peers get a random id with no durable mailbox and are reaped
when not present (`reaper.go:70-78`); no permission relay target; no way to
watch it or type into it; the daemon would own a new process supervisor. Fights
the whole "everything is a pane you can attach to" design.

**Recommendation: A.**

## Design (option A)

### 1. Agent definition (settings object)

New package `internal/agent`, same shape as `internal/harness`:

```
Agent {
  Name         string   // peer name, folder name, unique, [a-z0-9-]
  Icon         string
  Description  string   // one line; becomes the set_summary seed
  Instructions string   // long text; written to <home>/CLAUDE.md
  Harness      string   // default "claude"
  Account      string   // llm account, optional (existing per-pane route)
  Model        string   // optional, passed as --model
  AddDirs      []string // optional extra folders (--add-dir), e.g. the KB repo
  Window       string   // shared window to live in, default "Agents"
  Autostart    bool     // start on daemon boot
  Global       bool     // reachable by name from every group (default true)
}
```

Stored under settings key `agents`. `Reject` checks name shape, uniqueness, and
that the harness resolves. `Apply` is whole-list replace, and after saving it
rewrites `<home>/CLAUDE.md` for every agent whose instructions changed. Files
the user added under `knowledge/` are never touched.

Exposed as `agents` in `GET/PUT /v1/settings`. Presence of the key is the
capability signal for the Mac lens.

### 2. Agent home on disk

`~/.ccmux/agents/<name>/` with:

- `CLAUDE.md`, generated: role, the description, the instructions, and a fixed
  footer telling it how it is reached on the bus and where `knowledge/` is.
- `knowledge/`, user-managed reference files.
- `.claude/` is left to Claude itself (memory lands there under its own path).

The daemon owns the generated file only. A header line marks it as generated.

### 3. Start, stop, revive

- `POST /v1/agents/{name}/start`: find or create the workspace with
  `repoPath = home`, `kind = "agent"`, in the configured window; then spawn the
  harness pane with an agent launch line. If the workspace is cold, revive it.
- Launch line = harness command + ` --name <name>` + `--model` if set +
  `--add-dir` for each extra folder. Instructions stay in `CLAUDE.md`, not on
  the command line, so a revive needs nothing extra to know its role.
- Persisted startup command (not ephemeral), so revive already restarts it.
- Session continuity: phase 3 adds `--resume <id>` on revive using the id the
  hooks already stored. Until then a restart is a fresh session that re-reads
  `CLAUDE.md` and its memory, which is acceptable for a role agent.
- `Autostart`: on daemon boot, revive every autostart agent's workspace.
- Stop = archive the workspace. Delete agent = archive plus remove the folder,
  confirmed in the UI.

### 4. Peers bus changes (three, all additive)

1. **Global reach.** In `checkReachableLocked`, allow the send when the target
   peer's pane belongs to an agent workspace with `Global = true`. Replies flow
   back on the existing reply grant.
2. **Name fallback.** In `resolveTargetLocked`, when no match is found in the
   wanted group, look for a present peer whose name matches a global agent.
   This keeps the frozen error strings and group scoping for everything else.
3. **Spawn by agent.** In `trySpawnLocked`, before the folder guess, check the
   agent registry. If the name is an agent, revive and start it (same code path
   as `/start`), and match the returning peer by its pane id, not by repo path.

Nothing else in delivery, tasks, cursors or the permission relay changes. The
`serverInstructions` sentence "Peers are named after their repo directory" gets
one added sentence about agents. It is not frozen wording (`tools.go:14-21`
freezes only the SendMessage paragraph and the "Cannot send" prefix).

### 5. UI, both lenses in the same commit

Settings → **Agents** tab: list, add, edit, delete. Fields as in the struct.
Start/Stop button per row with a running state derived the same way in both
lenses: workspace with `kind = "agent"` exists and is live and its pane is not
at a bare shell.

Sidebar: an agent workspace shows the agent icon, hides the path and git
badges, and keeps the same open/close and window behaviour.

Web: `index.html:118-141` (tab + section), new `wireAgentSettings()` beside
`app.js:1520`, sidebar tweak in `wsRow` (`app.js:253`).
Mac: `DaemonAgent` + `supportsAgents` in `DaemonModels.swift`, `agentsTab` in
`DaemonSettingsView.swift`, `WorkspaceRow` icon/path branch in
`SidebarView.swift:908`. Swift cannot be built here; the release tag job builds
it.

### 6. Where the knowledge comes from

Three layers, all already supported by the CLI:

| Layer | Mechanism | Edited where |
|---|---|---|
| Role and rules | `CLAUDE.md` in the agent home | Agents tab, Instructions field |
| Reference docs | files in `knowledge/` | file explorer / file API |
| Live source | `--add-dir <path>` to a real repo | Agents tab, extra folders |

Plus Claude's own memory directory for what it learns while running.

## Phases and estimates (minutes of execution)

| Phase | What | Est. |
|---|---|---|
| 1 | `internal/agent` package, settings key, `agents` in GET/PUT settings, home folder writer, tests | 40 |
| 2 | `kind` column on workspaces, `/v1/agents/{name}/start|stop`, launch line, autostart on boot | 45 |
| 3 | Bus: global reach, name fallback, spawn by agent, shim instruction sentence, tests | 40 |
| 4 | Web lens: Agents tab, start/stop, sidebar styling | 35 |
| 5 | Mac lens: models, Agents tab, sidebar styling (unbuilt here) | 40 |
| 6 | `--resume` on revive for agent panes | 25 |

Phases 1 to 5 are one feature and should ship together. Phase 6 can follow.

## Test first (open questions)

1. Does `claude` inside `~/.ccmux/agents/x/` pick up that folder's `CLAUDE.md`
   and keep memory under a path keyed on that cwd? Expected yes, unverified.
2. Does the "trust this folder" prompt fire for a non-git folder, and does
   autoconfirm catch it? Autoconfirm matches the text, so likely yes.
3. Does `--resume <id>` accept the `session_id` the hooks report? Needed for
   phase 6 only.
4. Do we want agents to be one per host, or one per federation? Option A makes
   them per host, which is also where their folder is. The hub already
   aggregates workspaces across hosts, so they show up everywhere.

## Pros and cons of the feature itself

Pros
- Role agents with stable knowledge, reachable by name from any repo session,
  with delegation and permission relay for free.
- No new runtime: same tmux, same attach, same lenses.
- Knowledge is plain files on disk, so it is easy to inspect and back up.

Cons and risks
- Each running agent is a live Claude session. Idle it costs nothing, but a
  busy one shares the same account limits and failover pool as repo sessions.
- Global reach widens who can message whom. Keeping it a per-agent flag with a
  default we can flip mitigates it.
- Two lenses, one commit: the Mac side cannot be compiled on this host.
- Generated `CLAUDE.md` plus user files means two sources of truth in one
  folder. The generated header and the untouched `knowledge/` rule keep that
  clear.

---

# v2 (2026-09-04): base agents, instantiated per project

## The refined ask

Create an agent once (say "X Poster") with its skills, plugins, settings and a
base instruction file. Then, per project, choose "add X Poster to this
project". Later add it to another project. Questions that fall out:
project-specific memory, version drift of the base, not overwriting
project-specific data, and whether the Agent SDK is the right runtime for a
subscription login.

## Facts checked (Claude Code 2.1.260 on this host, docs read 2026-09-04)

Loading rules, from `code.claude.com/docs/en/memory`, `/skills`, `/plugins`:

- **CLAUDE.md** loads from the cwd and every parent directory, concatenated,
  root first. `--add-dir` folders' CLAUDE.md is NOT loaded unless
  `CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1`.
- **Skills** load from `~/.claude/skills/`, from `.claude/skills/` in the cwd
  and every parent up to the repo root, and from `.claude/skills/` inside
  each `--add-dir` folder. Plugin skills are namespaced `plugin:skill`, so a
  plugin skill and a project skill with the same name both stay available.
  Project or user `.claude/agents/` DO override a same-named plugin agent.
- **Plugins**: `--plugin-dir <folder>` loads a plugin for that session by
  reference. A plugin holds `skills/`, `agents/`, `hooks/hooks.json`,
  `.mcp.json`, `bin/`, and a `.claude-plugin/plugin.json` with a `version`.
  `/reload-plugins` picks up file changes in a running session.
- **Auto memory** lives at `~/.claude/projects/<project>/memory/` where
  `<project>` is derived from the **git repo**, so every cwd inside one repo
  shares one memory folder. `autoMemoryDirectory` in settings moves it, and it
  is honoured from `--settings <file-or-json>`.
- `--append-system-prompt-file`, `--settings`, `--name`, `--model`,
  `--add-dir`, `--resume` all exist in 2.1.260.

Runtime and login, from `code.claude.com/docs/en/agent-sdk/overview` and the
support article "Use the Claude Agent SDK with your Claude plan":

- SDK overview: "Unless previously approved, Anthropic does not allow third
  party developers to offer claude.ai login or rate limits for their products,
  including agents built on the Claude Agent SDK. Use the API key
  authentication methods."
- Support article, updated 2026-06-15: the planned per-plan SDK credit is
  **paused**; "Claude Agent SDK, `claude -p`, and third-party app usage still
  draw from your subscription's usage limits."
- **Channels** (what the peers bus uses to push messages into a session) are
  documented only for the interactive CLI (`--channels`,
  `--dangerously-load-development-channels`), are in research preview with an
  Anthropic allowlist, and the SDK's MCP page does not mention them at all.
- The SDK is a library around the same CLI. Everything ccmux relies on today
  (hooks socket, permission relay, autoconfirm, attach) assumes a pane.

**Conclusion on the SDK:** do not use it. The interactive `claude` CLI in a
tmux pane, logged in with the subscription and routed through the ccmux proxy,
is the path that is sanctioned, already works with the Max account, and is the
only documented host for channels. The SDK would lose the bus push and puts the
subscription question on shakier ground.

## The model

Two things, kept apart on disk. (Placement changed 2026-09-06: an instance is
its own session in the project's shared WINDOW, with its folder under
`~/.ccmux/windows/<window>/agents/<name>/`; the table below and the "no new
workspace kind" bullet describe the earlier per-repo placement. See
`agent-spec.md` §12 for what runs now.)

| | Base agent (template) | Instance (base × project) |
|---|---|---|
| What | A Claude Code **plugin folder** owned by ccmux | A **pane** in the project's workspace |
| Where | `~/.ccmux/agents/<name>/` | `<repo>/.ccmux/agents/<name>/` plus a memory folder |
| Written by | The Agents UI and the human | The human and the agent, never the daemon after creation |
| Versioned | `plugin.json` version, bumped on save | Records which base version it last started with |
| Peers name | n/a | `<name>` via `CLAUDE_PEERS_NAME` |
| Group | n/a | The project's shared window, like any pane there |

### Base agent folder

```
~/.ccmux/agents/x-poster/
├── .claude-plugin/plugin.json   name, version, description
├── AGENT.md                     base instructions (system prompt)
├── skills/<skill>/SKILL.md      how to post, how to reply, tone rules
├── agents/                      optional subagents
├── hooks/hooks.json             optional
├── .mcp.json                    optional, e.g. an X API MCP server
├── knowledge/                   reference docs, read on demand
└── agent.json                   ccmux fields: icon, model, account, flags
```

`AGENT.md` is passed with `--append-system-prompt-file`, so it lands in the
system prompt rather than a CLAUDE.md, and it never depends on cwd. The rest
rides in as a plugin via `--plugin-dir`.

### Instance folder in the project

```
<repo>/.ccmux/agents/x-poster/
├── CLAUDE.md            project-specific instructions for this agent
├── .claude/skills/      project-specific skills (bare names, no collision)
└── memory/              auto memory for this agent in this project (optional here)
```

The daemon creates this folder with a starter `CLAUDE.md` the first time the
agent is added to a project and **never writes there again**.

### Launch line (built by the daemon per instance)

```
CLAUDE_PEERS_NAME=x-poster env -u TMUX claude \
  --dangerously-load-development-channels server:claude-peers \
  --name x-poster \
  --plugin-dir ~/.ccmux/agents/x-poster \
  --append-system-prompt-file ~/.ccmux/agents/x-poster/AGENT.md \
  --add-dir <repo> \
  --settings ~/.ccmux/instances/<paneID>/settings.json
```

- cwd = `<repo>/.ccmux/agents/x-poster/`. That makes Claude load the
  instance `CLAUDE.md`, the repo's own `CLAUDE.md` above it, the instance
  skills, and the repo-root skills, all by the standard rules.
- `--add-dir <repo>` lets it edit project files without a prompt.
- `settings.json` per instance carries `autoMemoryDirectory`, and the model
  or permission defaults from `agent.json`. Generated at every start.
- The `VAR=value` prefix is already skipped by `StartupProgram`
  (`internal/harness/startupcmd.go:43-56`), so dormant detection and titling
  keep working.

## Answers to the four questions

### Project-specific memory

Three layers, each with one owner:

1. **Base knowledge**, shared by every project: `AGENT.md`, `skills/`,
   `knowledge/` in the base folder. Human-curated.
2. **Project instructions**, per project, in git: the instance `CLAUDE.md`
   and `.claude/skills/`. Human-curated, shared with teammates through the
   repo.
3. **Project auto memory**, per project, Claude-written: `autoMemoryDirectory`
   pointed at a per-instance folder. Default under `~/.ccmux/instances/`, with
   an option to keep it inside the instance folder in the repo so teammates
   and other hosts share it. Without this setting, the agent would share one
   memory folder with the project's main Claude session, because auto memory
   is keyed per git repo.

What is NOT native: cross-project learning ("things X Poster learned about X
in general"). Choice to make: give the base a writable `notes/` folder and
tell the agent in `AGENT.md` to append general learnings there, or keep
general knowledge human-only. Recommend the second until there is a reason.

### Version drift

The base is **loaded by reference at every start**, never copied into a
project. So the next start of any instance runs the latest base. For running
instances: skills, hooks and MCP servers reload with `/reload-plugins`, but
the system prompt is fixed at start. The daemon records the base version each
instance started with, and both lenses show a "base 1.3 available, running
1.2, restart" badge with a one-click restart. Saving the base in the UI bumps
the patch version automatically.

### Not overwriting project-specific data

Ownership is by folder, enforced by the daemon:

- Base folder: written by the Agents UI. Instances only read it.
- Instance folder: created once with a starter `CLAUDE.md`, then untouched by
  the daemon. Only humans and the agent write there.
- Skills cannot collide: plugin skills are namespaced (`x-poster:post`),
  project skills are bare. Subagents in `agents/` CAN be overridden by a
  same-named file in the instance's `.claude/agents/`, which is the intended
  override hook, so the UI should say so.
- Memory folder: Claude-owned, never regenerated.

### Subscription and the Agent SDK

Covered above: stay on the interactive CLI in a pane. It is the sanctioned
subscription path, it is what ccmux already proxies, and it is the only place
channels are documented. Cost: every instance is a live session drawing from
the same plan limits and the same failover pool.

## What changes in ccmux (smaller than v1)

- **No new workspace kind, no bus reach rule.** An instance is a pane inside
  the project's workspace, so its group is the project's window and its name
  comes from `CLAUDE_PEERS_NAME`. Same-group messaging, delegation and the
  permission relay need nothing.
- `internal/agent`: base folder registry (list, create, save, bump version,
  validate), instance folder bootstrap, launch-line builder, per-instance
  settings writer. Reuses the harness `Store` pattern for `agent.json` fields
  that the UI edits.
- `panes`: new column `agent TEXT` (which base) and `agent_version TEXT`
  (what it started with). Revive replays the stored launch line, which is
  rebuilt from the current base at each start so drift resolves on restart.
- API: `GET/PUT /v1/settings` gains `agents` (base list); `POST
  /v1/workspaces/{id}/panes` accepts `{agent: name}`; `POST
  /v1/agents/{name}/bump` or implicit bump on save.
- Bus, one change: `spawn_if_missing` for a name that matches an agent in the
  sender's group with no present peer starts that instance instead of
  guessing a folder (`internal/peers/spawn.go:66-82`). Match the returning
  peer by pane id.
- Web lens: Agents tab (instructions editor, skill list with "open folder",
  version), workspace menu "Add agent ▸", pane tab icon and drift badge.
- Mac lens: the same three surfaces, plus `DaemonPane.agent` decoding, which
  the Mac side lacks today (`DaemonModels.swift:424`). Unbuilt on this host.

## Estimates (minutes of execution)

| Phase | What | Est. |
|---|---|---|
| 1 | `internal/agent` package, base folder layout, settings key, tests | 45 |
| 2 | Pane columns, spawn-by-agent, launch line, instance bootstrap, per-instance settings | 45 |
| 3 | Bus spawn-by-agent + shim wording, tests | 25 |
| 4 | Web lens: Agents tab, add-agent menu, badges | 40 |
| 5 | Mac lens: same, unbuilt here | 40 |
| 6 | `--resume` on revive for agent panes (needs `pane_sessions` id) | 25 |

## Test first

1. Launch by hand with the line above in a repo subfolder and run `/context`:
   confirm the instance `CLAUDE.md`, the repo `CLAUDE.md`, the plugin skills
   and the appended system prompt all show.
2. Confirm `autoMemoryDirectory` from `--settings` is honoured (docs say yes).
3. Confirm the "trust this folder" prompt for the instance folder is caught
   by autoconfirm.
4. Confirm `CLAUDE_PEERS_NAME` on the shell line reaches `ccmux-peers` and the
   peer lists as `x-poster` in the project's group.

---

# v2 addendum (2026-09-04): discovery, liveness, history, harness choice

## How other sessions learn that "X Poster" exists

Two surfaces, both fed from the daemon's agent list so they are never stale:

1. **`list_peers`** (live, every call): the daemon's `/v1/peers/list` reply
   gains an `agents` section listing, for the caller's group, every agent
   instance added to that project (running or dormant) and every base agent
   not yet added, each with its one-line description and a status:
   `running`, `dormant (starts on contact)`, `not added (added on contact)`.
   The shim renders it under the peers (`cmd/ccmux-peers/tools.go:240-268`).
2. **Server instructions** (snapshot at session start): the shim fetches the
   same catalog at MCP init and appends one paragraph: "Agents on this bus:
   x-poster (posts to X), kb-writer (...). Message one with
   `send_message to_name=<agent> spawn_if_missing=true`; it starts if it is
   not running. `list_peers` shows the live list." The frozen wording in
   `serverInstructions` is untouched; this is appended after it.

The description shown is the base `agent.json` description. On startup the
instance calls `set_summary` with the same text, so a running agent also
shows its role in `from_summary` on every message it sends.

## Keeping it live and spinning up on contact

An instance is a persisted pane in the project's workspace, so:

- **Workspace open** → the pane runs `claude` and sits idle at no cost.
  Contact by name is delivered instantly.
- **Claude exited, pane at shell** (dormant) → `spawn_if_missing` finds the
  dormant agent pane and calls `StartHarnessInPane` with the agent launch
  line and the birth prompt (`manager.go:274-305`), instead of spawning an
  ephemeral pane. The queued message replays as a channel message once the
  peer registers, same as today (`spawn.go:136-161`). Cold start is roughly
  5 to 10 seconds; the sender gets `spawning: true` right away.
- **Workspace closed/archived** → contact from another group with
  `to_group` revives the workspace (`ReviveWorkspace`, `manager.go:417`),
  which replays every pane's startup command, and the agent comes back.
- **Always-on** → optional `keepAlive` flag per instance: revive on daemon
  boot and restart the harness if it exits. Off by default.

`SpawnTimeout` (60s) and the timeout/abort messages apply unchanged.

## A pane with history, like a regular Claude window

It is a regular Claude window. Each instance is an interactive `claude` in a
tmux pane, so scrollback, the conversation, every bus message it received
(shown as `<channel>` tags) and every command it ran are visible in the pane
and in Claude's own transcript.

Across a daemon restart, the conversation itself comes back with resume.
Because each instance has its own cwd, `claude --continue` means "the most
recent conversation in this agent's folder", which is exactly this agent's
last session. That avoids tracking session ids. **Unverified:** what
`--continue` does on the very first start with no conversation. The test
could not run here because the shell outside a pane has an expired login.
Fallback if it errors: the daemon uses `--resume <id>` from
`pane_sessions.live_ids` when it has one and a plain start otherwise.

Separately, every bus message is already stored durably in `peer_events`
(`store/store.go:104-111`), so a per-agent message log in the lenses is
possible later without touching the harness.

## Claude Code vs Hermes Agent vs pi as the agent harness

Checked 2026-09-04 against each project's own docs and Anthropic's policy.

| | Claude Code CLI | Hermes Agent | pi |
|---|---|---|---|
| Subscription (Max) allowance | Yes, first-party | No. Hermes docs: OAuth path "only works on a Claude Max plan with purchased extra usage credits — the base Max allowance is never consumed"; Pro cannot | No. pi's provider layer follows Anthropic's policy; community OAuth extensions exist "at your own risk" of an account ban |
| API key / OpenRouter / Ollama via ccmux proxy | Yes | Yes | Yes |
| Bus push (channels) | Yes, what ccmux uses today | No channel concept documented; MCP client yes, so poll-only bus is possible | No MCP built in; needs an extension |
| Hooks socket, permission relay, autoconfirm in ccmux | Wired | Not wired | Not wired |
| Persistent memory | Auto memory per repo, redirectable | Yes, plus auto-generated skills | Session trees, AGENTS.md |
| Scheduling, chat gateways (Telegram etc.) | No | Yes | No |
| Pane with history | Yes | CLI mode exists | TUI |

Anthropic banned subscription OAuth in third-party tools in February 2026
and has enforced it server-side since 2026-04-04: a third-party harness with
a subscription token gets a 400 and is billed as extra usage per token. The
ccmux proxy already refuses to pair the Max account with a non-claude
harness for this reason.

**Recommendation:** Claude Code as the agent harness. It is the only one
that runs on the subscription's included allowance and the only one with
channels, and everything ccmux has built (hooks, relay, autoconfirm, proxy
failover) assumes it. Hermes' extras (cron, chat gateways) are real, but
they cost the subscription and the push bus.

Keep `harness` on the base agent anyway. A future base could run Hermes or
pi on an API-key or Ollama account and join the bus poll-only: the shim
already supports poll-only registration (`registry.go:31-33`), and Hermes
speaks MCP. That is a later experiment, not part of this feature.

On the Agent SDK for personal use: the "third-party developers" note is
about offering login to others, and Anthropic's support article says SDK and
`claude -p` usage draw from your own plan limits. So personal SDK use is
allowed. It is still the wrong tool here: channels are not documented for
it, and it gives no pane, so the history question would need a custom UI.

---

# v3 (2026-09-04): agents do not stay running

## The problem with 40 long-lived panes

Claude Code ships almost daily. A running session keeps the version it
started with, and so does the ccmux-peers shim it loaded and the base agent
system prompt it read. With 40 interactive agents that means 40 manual
restarts per update, and 40 processes of roughly 300 to 500 MB each sitting
idle. The v2 "instance is a persisted pane that stays up" model breaks here.

## How others do it (checked 2026-09-04)

| Project | Process model | Versions and restarts |
|---|---|---|
| desplega-ai/agent-swarm | Lead agent persistent; **workers are Docker containers spawned per task**; memory via a `memory-store` tool in SQLite | "pause/resume across deploys"; child tasks inherit a bounded prior-task context preamble |
| Untrivial-ai/agent-orchestrator | "A worker is AO's unit of execution: **one task, one coding agent, one isolated workspace**"; 26 harnesses | Reopen a worker later to continue; no version story needed because nothing idles |
| andyrewlee/amux | **Long-lived tmux sessions**, same as ccmux today; no agent messaging | Nothing on versions or restarts. Same gap we have |
| HKUDS/DeepCode | Agents **spawned per turn** inside durable local Sessions; tool calls part of the record | Preset composition "snapshotted into the session record and locked once the conversation starts" |
| VRSEN/agency-swarm | In-process objects, **instantiated per interaction**; threads persisted via callbacks | Nothing to restart; state is in the thread store |

Four of five never keep an agent process alive between tasks. State lives in
a store, the process is spawned when there is work. The one that keeps
processes alive (amux) has the same unsolved restart problem we are worried
about.

## The fix: pane is durable, process is not

Keep everything from v2 (base plugin folder, instance folder, per-project
memory, bus naming, history in the pane) and change one rule:

**An agent process runs only while it has work.**

- **Start on contact.** `spawn_if_missing` starts the harness in the agent's
  pane. Every start reads the current `claude` binary, the current shim and
  the current base folder. That is the whole update story: there is nothing
  to restart, because nothing is running.
- **Exit when idle.** The daemon watches the pane's Stop hook and its open
  `peer_tasks`. After `idleExit` (default 10 minutes) with no open task and
  no pending message, it ends the session (types `/exit`). The pane drops to
  a shell and stays: scrollback and the transcript remain in place.
- **Resume on the next start.** `--continue` in the agent's own folder
  reopens its last conversation, so it remembers the thread. An instance can
  instead opt for `fresh` starts and rely on auto memory, which is cheaper
  on context for agents that answer many small unrelated requests.
- **Cap concurrency.** A daemon-wide `maxRunningAgents` (default 6) bounds
  RAM. Contacts beyond the cap queue in the existing pending-spawn table and
  start as others exit; the sender still sees `spawning: true`.

Cost: a cold start of 5 to 10 seconds on first contact after idle. For a
knowledge-base writer or an X poster that is fine. For anything that must
answer inside a second, mark it `keepAlive`.

## The few that stay up: rolling restart owned by the daemon

For `keepAlive` instances (expected: a handful, not 40):

- Each start records the `claude` version and base version on the pane.
- A timer compares them with `claude --version` and the base manifest.
- On drift, the daemon restarts idle ones immediately and busy ones as soon
  as their open tasks close and the Stop hook fires. `--continue` carries the
  conversation over. Both lenses show "restarting for 2.1.261" on the pane.
- Manual "Restart all agents" button exists for the impatient.

A running old version is not broken, so this can be lazy. Only shim and base
changes are ours to force, and the daemon knows when those happen.

## What this changes in the phase list

- Phase 2 gains: idle-exit watcher, concurrency cap, `keepAlive`,
  version stamps on panes, rolling restart. About 40 minutes more.
- Phase 6 (`--continue` or `--resume` on start) moves into phase 2, since
  resume is now the normal path, not a nicety.
- Test first, item 5: `--continue` with no prior conversation in the folder
  must either start fresh or be detected and retried plain. Still unverified
  on this host.

## v3 amendment (2026-09-05): fresh start per task, not `--continue`

`--continue` reloads the whole previous conversation on every start. For an
agent that wakes for many small unrelated requests that is a token hog and it
bleeds one task's details into the next. Default is therefore **fresh**:

- Every start is a new conversation. The same system prompt, plugin and
  CLAUDE.md prefix means prompt caching does most of the work.
- A session lives for the whole task chain: it exits only after the idle
  timeout with no open `peer_tasks` and no pending message, so a multi-message
  exchange with a peer stays in one conversation.
- What carries over between tasks: auto memory per instance (Claude-written
  learnings, first 200 lines loaded), the instance `CLAUDE.md` and skills, the
  base folder, and a short `log.md` the agent appends one line to per task
  ("2026-09-05 posted thread about v0.1.38, 3 posts") so it can answer "what
  did you do last time" without the transcript.
- `--continue` stays available per instance as an opt-in for agents whose
  work is one long thread.

## Stacks of the five projects (checked 2026-09-05 via repo metadata, README, manifests)

| Project | Language / runtime | Storage | How agents run | LLM access | Agent to agent | UI |
|---|---|---|---|---|---|---|
| desplega-ai/agent-swarm | TypeScript on Bun or Node; Hono HTTP API | SQLite (sqlite-vec) or Postgres | Docker container per task; persistent lead | Raw `@anthropic-ai/sdk`, `openai`, `@openai/codex-sdk`, and **pi embedded as a library** (`@earendil-works/pi-agent-core`, `pi-coding-agent`); API keys | MCP tools + HTTP API + Slack (Bolt) | React dashboard, Slack |
| Untrivial-ai/agent-orchestrator | Go 1.25 daemon (chi, coder/websocket, cobra) | SQLite WAL (modernc, sqlc, goose) | **Real CLIs in PTYs**: native detached PTY host on macOS, **tmux on Linux** (bundled), ConPTY on Windows; `creack/pty`, `vt-go` screen model; one worker = one task = one worktree | Whatever the CLI uses (26 agents incl. Claude Code) | Orchestrator only; workers do not message each other | Electron + TypeScript desktop, Kanban |
| andyrewlee/amux | Go TUI | JSON files in `~/.amux` | Long-lived CLIs in tmux 3.2+, git worktrees | Whatever the CLI uses | None | TUI |
| HKUDS/DeepCode | Python 3.12 (uv); Tauri 2 desktop (Rust + TS) | SQLite sessions, local project files | Python agent loop per turn inside durable sessions; can delegate to Codex or Claude Code as subagents | Raw APIs: OpenRouter, OpenAI, Anthropic, DeepSeek, Gemini, Ollama, vLLM; MCP client | Coordinator plus `spawn_agent` subagents | CLI TUI, Tauri desktop, headless |
| VRSEN/agency-swarm | Python 3.12 on the OpenAI Agents SDK | Threads via `load_threads_callback` / `save_threads_callback` | In-process objects per interaction | OpenAI native; others through LiteLLM | `send_message` tool with directional `ceo > dev` flows | Copilot web demo, TUI, FastAPI |

Two camps:

- **Own-the-loop frameworks** (agent-swarm, DeepCode, agency-swarm): they
  call the model API themselves with API keys. No subscription, per-token
  billing, full control over every turn.
- **CLI-in-a-PTY managers** (agent-orchestrator, amux): they run the vendor's
  CLI in a terminal, same as ccmux. Subscription-friendly. agent-orchestrator
  is the closest relative: Go daemon, SQLite, PTY or tmux runtime, Electron
  lens, one worker per task, no long-lived idle agents.

None of the five keeps dozens of idle agent processes. ccmux v3 lands in the
second camp with the per-task lifecycle of agent-orchestrator plus the bus
neither of them has. agent-swarm's use of pi as an embedded library is the
reference if a non-subscription harness is ever wanted on the bus.

---

# v4 (2026-09-05): harness-neutral base, adapters per harness

## Why

The user may move from a Claude subscription to an OpenAI one. Anything
Claude-only in the agent definition would then have to be rewritten. The
definition must be plain files every harness reads; only the launch adapter
may know which harness it is.

Installed on this host today: claude 2.1.261, opencode 1.18.23, pi 0.84.3,
codex 0.149.1. Hermes is not installed. The ccmux proxy already pairs codex
with a ChatGPT login and it is verified live (memory, 2026-08-25), so the
OpenAI-subscription path exists right now on the codex harness.

## What each harness reads (checked 2026-09-05)

| | Claude Code | opencode | codex | pi | Hermes |
|---|---|---|---|---|---|
| Instructions | `CLAUDE.md` (imports `@AGENTS.md`) | `AGENTS.md`, falls back to `CLAUDE.md`; `instructions` list in `opencode.json` | `AGENTS.md` | `AGENTS.md` | own config |
| Skills | `.claude/skills/*/SKILL.md`, plugins | `.opencode/skills`, **`.claude/skills`**, `.agents/skills`, global equivalents | n/a | n/a | own skills |
| MCP client | yes (`~/.claude.json`, `.mcp.json`) | yes (`opencode.json` → `mcp.<name>.{type:"local",command:[...],environment}`) | yes | no, extension needed | yes |
| Push into session | channels (research preview, dev flag) | **HTTP API**: `--port N`, then `POST /session/:id/prompt_async` or `/tui/append-prompt` + `/tui/submit-prompt` | none known → poll | extension | gateway or poll |
| Resume | `--continue`, `--resume <id>` | `--continue`, `--session <id>`, `--fork` | own | sessions | own |
| Subscriptions | Claude Pro/Max | **ChatGPT Plus/Pro**, GitHub Copilot; Claude login removed in 1.3.0 ("Anthropic explicitly prohibits this") | ChatGPT | none | Max extra-usage credits only |
| Open source | no | yes, MIT | yes | yes | yes, MIT |

## The neutral agent definition

```
~/.ccmux/agents/x-poster/
├── agent.json        ccmux: name, icon, description, harness, account, model, flags
├── AGENTS.md         base instructions, plain markdown
├── CLAUDE.md         one line: @AGENTS.md          (Claude Code reads this one)
├── skills/*/SKILL.md standard skill folders
├── mcp.json          MCP servers the agent needs, neutral shape {name: {command, args, env}}
└── knowledge/        reference files
```

Instance folder in the repo, same idea:

```
<repo>/.ccmux/agents/x-poster/
├── AGENTS.md  +  CLAUDE.md (@AGENTS.md)     project-specific instructions
├── .claude/skills/                          project-specific skills (Claude and opencode both read this path)
├── memory/                                  agent-owned notes: MEMORY.md index + topic files
└── log.md                                   one line per task
```

**Memory is agent-owned, not harness-owned.** `AGENTS.md` tells the agent to
read `memory/MEMORY.md` at start and write learnings there. That works in
every harness. Claude's auto memory becomes a bonus, not the mechanism.

## Adapters (extend the existing harness objects)

Each `harness.Harness` gains a small adapter block the daemon uses to build the
launch:

- **instructions**: Claude → `--append-system-prompt-file base/AGENTS.md`;
  opencode → write `instructions: [base/AGENTS.md]` into a per-instance
  `opencode.json`; codex/pi → symlink or include `AGENTS.md` at cwd.
- **skills**: Claude → `--plugin-dir base`; opencode → symlink
  `.agents/skills → base/skills` in the instance folder.
- **mcp**: Claude → user-scope registration already done; opencode → the
  `mcp` block in the per-instance `opencode.json`, including `ccmux-peers`
  with the pane env; codex → its MCP config; pi → later.
- **push**: `channel` (Claude), `http` (opencode: daemon picks a port,
  passes `--port`, shim or daemon POSTs inbound messages), `poll` (codex,
  anything else). The shim already has a poll-only mode
  (`registry.go:31-33`).
- **resume**: flag template per harness; default fresh.
- **account kinds**: already exist (`Harness.AccountKinds`).

## opencode versus Claude Code, for this use

- **Portability**: opencode wins. ChatGPT Plus/Pro and Copilot logins are
  native, 75+ providers, reads Claude-format skills and CLAUDE.md, open
  source. Claude subscription is refused by design.
- **Peer talk**: buildable, and on a public API rather than a research
  preview. The daemon launches `opencode --port N`, the shim registers as
  usual (MCP tools work unchanged), and inbound messages are POSTed to
  `/session/:id/prompt_async` or the `/tui` endpoints. Plugins
  (`@opencode-ai/plugin`, events like `session.idle`, `message.updated`)
  can report idle and completion back to the daemon, which replaces the
  Claude hooks socket for opencode panes.
- **What Claude Code has that opencode lacks**: the Claude subscription
  allowance, shell hooks, auto memory, first-party plugin marketplace,
  channels. Everything ccmux built on hooks needs a plugin equivalent on
  opencode.
- **Verdict**: Claude Code stays the default harness while the Claude
  subscription is the main account. opencode is the second harness to wire
  and the natural OpenAI-subscription path alongside codex.

## The bridge projects

`pi-claude-bridge` and `opencode-with-claude` (Meridian) run Claude Code or
the Agent SDK as a backend so another UI can use the subscription.
`hermes-claude-auth` copies Claude's credentials into Hermes. All three are
unofficial, admit cache loss or fragility, and sit on the wrong side of
Anthropic's policy for anything but personal use. Not a base to build on.
Hermes' own Claude Code skill is the honest version of the same idea: it
drives `claude -p` or a tmux `send-keys` session as a subprocess.

## Cost of neutrality

Roughly 60 to 90 minutes more than a Claude-only build: the adapter block,
the per-instance `opencode.json` writer, the `http` push path, and an
opencode plugin for idle/complete signals. Everything else in v3 is
unchanged. Wire Claude and opencode first, codex poll-only, pi and Hermes
when there is a reason.

---

# v5 (2026-09-05): opencode as the one harness, Meridian for Claude

## What Meridian is (README read 2026-09-05; 2.0k stars, 955 commits, MIT)

A local HTTP proxy on `127.0.0.1:3456` that speaks the Anthropic Messages API
and OpenAI chat completions, and turns every request into a Claude Agent SDK
`query()` call, so traffic reaches Anthropic through the first-party SDK and
the user's Claude login. Sessions are reused across requests ("persist across
requests, survive compaction and undo, resume after proxy restarts"), SSE
streaming works, tool calls can be passed through to the client, prompt cache
efficiency is shown on `/telemetry`, and multiple Claude accounts can be
switched with sticky routing. `opencode-with-claude` is a plugin that starts
Meridian with opencode and points the Anthropic provider at it.

## What we would be taking on

1. **A community proxy in the hot path of every Claude agent.** One process,
   one point of failure, one project to track. The daemon can supervise it
   like the dev servers it already runs.
2. **Policy exposure.** Meridian rides the Agent SDK's subscription billing,
   which Anthropic announced changes to and then paused on 2026-06-15. Its own
   FAQ records a server-side "Extra Usage classifier" that blocked
   tool-bearing requests until July 2026. A flip there stops every Claude
   agent at once. Claude Code run directly has no such exposure. For personal
   use the terms are fine today.
3. **Two agent loops.** opencode runs its loop; each turn becomes an SDK
   `query()`, which is itself Claude Code's loop with its own tools and
   system prompt. Meridian says it maps tools into SDK custom tools and does
   not disable Claude Code's own tools. Must be checked in practice: no
   double system prompt, no Claude-side tool firing in the proxy's cwd,
   acceptable cache hit rate.
4. **The ccmux proxy is bypassed** unless Meridian's environment carries
   `ANTHROPIC_BASE_URL` to the pane proxy, so the SDK-spawned `claude` still
   routes through it. Then account failover, usage display and aliases keep
   working. To verify.
5. **Token refresh.** OAuth tokens expire in about 8 hours; Meridian
   refreshes, but "may fail after weeks of inactivity, requiring `claude
   login`". The daemon should surface that as an account status.

## Is opencode good for spin-up-on-demand agents

Yes, and better than a TUI-per-agent model, because it is client-server:

- **One `opencode serve` per host, N sessions.** An agent instance is a
  session in that server, not a process. "Spin up" means `POST /session`
  once, then `POST /session/:id/prompt_async` with the message. Zero idle
  processes, no cold start after the first, and `--continue`/`--session`
  semantics are the server's own.
- **The pane is a viewer.** The pane runs `opencode attach <url>` on that
  session only while a human is looking. History is the session, stored in
  opencode's SQLite, exportable with `opencode export`. To verify: `attach`
  with `--session` opens a specific session.
- **Events replace hooks.** `GET /event` (SSE) gives `session.idle`,
  `message.updated` and the rest, so the daemon knows busy/idle without a
  hooks socket. A small plugin is optional.
- **Per-agent identity** is the `--agent` name and a per-agent `opencode.json`
  (instructions, skills, MCP incl. `ccmux-peers` with the instance's env).

Per-task process lifecycle from v3 collapses into per-task sessions. The
concurrency cap becomes a cap on concurrently active sessions.

## "Start typing and it spins up"

The agent's pane is its inbox. When the agent is asleep the lens shows a
composer over the pane ("Message X Poster…"). Enter sends the text to the
daemon, which does exactly what a peer contact does: create or reuse the
session, post the prompt, and turn the pane into a live view. While the agent
works the pane is a normal terminal. When it goes idle the composer comes
back on top of the history. Same code path for humans and peers, same in both
lenses. The existing harness bar over shell panes is the place this goes
(`daemon/web/app.js:948`, Mac `PaneTabBar`).

## Recommendation

Keep the neutral base from v4. Pick the Claude path with a **30-minute
spike** before committing:

1. Start Meridian and one opencode session in a pane with the Anthropic
   provider pointed at it and `ANTHROPIC_BASE_URL` set to the pane proxy.
2. Give it a task that uses Read, Edit and Bash. Confirm tools run in the
   pane's cwd through opencode, not through Claude Code inside the proxy.
3. Read `/telemetry` for cache hit rate over a 10-turn exchange.
4. Kill Meridian mid-task and restart it. Confirm the session resumes.

If all four pass: opencode is the single harness, Meridian is a
daemon-supervised sidecar, Claude Code stays as a one-line fallback harness.
If any fails: Claude Code direct for Claude agents, opencode for OpenAI, both
on the same neutral base, which was the v4 plan.

## v5 addendum: Meridian does not replace the ccmux proxy

They solve different problems.

| | ccmux proxy (`internal/llmproxy`, ~2k lines) | Meridian |
|---|---|---|
| Job | Route any pane's model traffic to any account | Make a Claude login usable from a non-Claude-Code harness |
| Upstreams | Anthropic API key, Claude setup-token, OpenAI bearer, codex/ChatGPT pass-through, Ollama, any OpenAI-compatible URL | Claude via the Agent SDK only |
| Routing | Per pane, global default, harness↔account-kind pairing (`api/llmroute.go:68`) | Per Meridian profile, sticky sessions |
| Failover | Claude pool: replay on 429, limited-until from usage headers (`llmproxy/pool.go`) | Multi-profile switch |
| Compat | `role:"system"` downgrade for non-Anthropic upstreams, per-account model aliases (`llmproxy/compat.go`) | n/a |
| Codex | Websocket bypass fixed by forcing HTTP through the proxy (`harness.go:115-120`) | n/a |
| Lenses | Accounts tab, usage display, per-pane route menu in both | Its own `/telemetry` page |

Ditching the proxy would drop Ollama, OpenRouter, codex with a ChatGPT login,
per-pane routing, failover and the Accounts tab, which is the opposite of
keeping options open. Meridian cannot do those, and the proxy cannot do the
one thing Meridian does: it cannot hand a subscription token to opencode,
because Anthropic detects third-party callers server-side. Only the
first-party SDK path gets through.

**Shape: Meridian becomes an account kind behind the proxy.** New kind
`meridian` with `BaseURL` on loopback; opencode panes route to it like any
other account; `kindAllowed` permits `meridian` for opencode and pi and
refuses it for the claude harness, which keeps using the setup-token path.
The daemon supervises the Meridian process the way it supervises dev
servers, and reads `/telemetry` for the Accounts tab. The Claude failover
pool is skipped for `meridian` (Meridian's profiles own that). Meridian's
own SDK-spawned `claude` talks straight to Anthropic; do not point it back at
the pane proxy, which would create a loop and a pane-less route.

Estimate: account kind, pairing rule, supervisor, status read, both lenses'
kind lists: about 45 minutes.

## Spike results (run 2026-09-05, about 25 minutes of execution)

Setup: Meridian 1.67.0 (`npm install -g @rynfar/meridian`, linked into
`~/.local/bin`), started with `MERIDIAN_PASSTHROUGH=1` and the ccmux-stored
Claude setup token as `CLAUDE_CODE_OAUTH_TOKEN`, because the host's own
`claude login` had expired. opencode 1.18.23 in a scratch git folder with
`opencode.json` pointing the anthropic provider at `http://127.0.0.1:3456/v1`.
Model `anthropic/claude-sonnet-5`. No ccmux pane involved; `opencode run`
from a shell, which is the same engine.

| # | Test | Result |
|---|---|---|
| 1 | Meridian answers a raw Anthropic-style request on the subscription token | Pass. `/health` reports `loggedIn: true`, mode passthrough |
| 2 | Read, Edit, Bash task: tools run in opencode in the pane cwd, not inside the proxy | Pass. Edit diff and `wc -l` ran in the scratch folder; Meridian logged `tools=10` forwarded. 18 s for a 4-request task |
| 3 | Five follow-up turns as **separate opencode processes** (`--continue`) | Pass. 5 to 10 s per turn including opencode start; Meridian kept the session lineage; 99 to 100% cache read on those requests |
| 4 | Kill Meridian mid-task, restart, continue | Pass. The in-flight turn failed with "Cannot connect to API" (exit 1) after the first file; after restart, `--continue` remembered what was done and finished the other four files; first request after restart hit 99% cache |

Findings that matter for the design:

- **Cache pattern, solved (rerun 2026-09-05 afternoon).** Inside a
  multi-request task the requests alternated 99% and about 45%, the low ones
  re-writing 8 to 12k tokens. `meridian setup` (global
  `~/.config/opencode/opencode.jsonc` now lists the Meridian plugin; health
  shows `plugin.opencode: configured`) did **not** change it. Isolation runs:
  creating files with the write tool → alternating; creating files with
  `bash echo` → alternating; editing an existing file six times → 99%
  throughout. So the trigger is "a new file appeared in the project", not
  the tool. opencode 1.18.23's system prompt has no file tree (checked in the
  binary), but its `snapshot` feature "tracks file changes during agent
  operations". With `"snapshot": false` in `opencode.json` the same
  new-files task ran at 98, 97, 99, 95, 99, 94, 99%. Agent instances must
  ship `snapshot: false`. Bonus: the first request of that fresh session hit
  98% because the system-prompt prefix was still warm from the previous
  session in the same folder, which is exactly the on-demand case.
- **Meridian overhead** (its `/telemetry/summary`, 33 requests): proxy
  overhead p50 55 ms, p95 244 ms; queue wait p50 1 ms; zero errors.
  `/telemetry/requests` and `/telemetry/logs` are JSON and are what the
  daemon should read for the Accounts tab.
- **Health lies about auth.** `/health` reads `~/.claude/.credentials.json`
  (expired here, `renewalRequiredSoon: true`) while requests succeed on the
  env token. The daemon must judge Meridian by a probe request, not `/health`.
- **Failure mode is clean.** A dead proxy surfaces as an opencode error for
  that turn; nothing hangs; history stays in opencode's session store.
- **Cold start is small.** A no-tool turn is 5 to 6 s end to end including
  opencode process start and model time.
- **Passthrough is the right mode.** Tools ran in opencode. Without
  `MERIDIAN_PASSTHROUGH=1` the SDK would execute tools in the proxy's cwd.
- Meridian's bundled `@anthropic-ai/claude-code` needs npm postinstall
  scripts allowed (`--allow-scripts=@rynfar/meridian,@anthropic-ai/claude-code`).

Verdict against the v5 criteria: all four pass, and the one cost anomaly has
a one-line fix on the opencode side. Nothing blocks the "one harness" shape.

State left on the host: Meridian installed globally and linked into
`~/.local/bin`; `~/.config/opencode/opencode.jsonc` now carries the Meridian
plugin entry (written by `meridian setup`, previously only the `$schema`
line); the Meridian process is stopped; scratch files live under the session
scratchpad only. No ccmux code or settings changed.

---

# Next steps (proposed 2026-09-05)

Order chosen so each step is useful on its own and de-risks the next.

| # | Step | Why first | Est. |
|---|---|---|---|
| ✓ | Step 0 shipped 2026-09-05 (`ffd0946`); step 1 the same day (`0a38558`, `internal/agent`, `/v1/agents`); step 2 the same day (`59d7b5b`, `8b8bcf7` instances + the lifecycle loop); step 3 the same day (bus: agents catalog in list_peers and the instructions, spawn-by-agent into the sender's workspace, TUI push of bus messages into opencode instances). Verified end to end from a live session: contact by name → started → answered over the bus. Step 4 (web lens: Agents tab, agent rows in the workspace menu, composer over a sleeping agent pane with the history below, ⚙ tab mark, drift note) the same day. Step 5 (Mac lens: Agents tab with per-card save and confirmed delete, agent rows in the workspace context menu, a prompt box for the first message, state from the daemon) the same day, unbuilt on this host. | | |
| 0 | **opencode as a real harness on the Claude subscription.** Account kind `meridian`, pairing rule, Meridian supervised by the daemon, `snapshot: false` and the Meridian plugin written into a per-pane opencode config, telemetry read for the Accounts tab, both lenses' kind lists. | Independently valuable today; exercises every adapter piece the agents need; no agent code involved. | 60 |
| 1 | **Agent base package** (`internal/agent`): folder layout from `agent-spec.md`, `agents` settings key, validation, version bump, per-instance config writer for the opencode adapter. Tests. | Pure daemon, file-based. Agents can be created by editing files before any UI exists. | 45 |
| 2 | **Instances and lifecycle**: pane columns, start-with-prompt, idle exit, concurrency cap, keepAlive, version stamps, rolling restart. | Makes an agent runnable from a curl. | 60 |
| 3 | **Bus**: catalog in `list_peers`, appended instruction paragraph, spawn-by-agent, http push for opencode. | Makes agents reachable by other sessions. | 45 |
| 4 | **Web lens**: Agents tab (create, edit, version, start/stop), "Add agent ▸" on a workspace, composer over a sleeping agent pane, drift badge. | The Mac app follows the same endpoints. | 60 |
| 5 | **Mac lens**: same surfaces. Unbuilt on this host. | Same-commit rule. | 60 |

Plan, then implement, per step: each step gets its callers listed and its
blast radius stated before code, since every one touches shared contracts.
Step 0 can start now.

---

# Deferred: provider-neutral workflows and model routing (2026-09-10)

Decision: keep opencode on one stable ccmux-routed provider and switch the
account behind the pane proxy. Do not make users change opencode providers when
moving between Claude and OpenAI. Ported qa and solo-workflow agents should not
pin vendor models; they inherit the session's logical model tier instead.

The intended model contract is provider-neutral (`quality`, `balanced`,
`fast`). Each account maps those tiers to its own models, for example `quality`
to Opus on Claude and the preferred reasoning model on OpenAI. The existing
per-account aliases already run after account selection, so the mapping belongs
there rather than in every skill or subagent.

Current limit: hosted opencode panes expose the proxy as an Anthropic Messages
provider. Meridian adapts a Claude subscription to that surface, but the proxy
does not translate Anthropic Messages to native OpenAI Responses. An OpenAI
switch is seamless today only when its upstream exposes an Anthropic-compatible
surface. Native OpenAI subscription support needs an equivalent adapter that
presents the same stable surface to opencode.

## Proposed implementation plan

This stays behind the current task list. Each phase is a separate save point;
do not start production integration unless the spike passes every hard gate.

### Phase A: dependency decision and isolated spike (45-60 minutes)

Use a pinned CLIProxyAPI release as an external process, not its Go SDK. Run one
process per OpenAI subscription account with its own loopback port, config,
random local API key and private auth directory. That keeps account identity,
quota and failure isolation aligned with ccmux's existing account model.

Before using it, confirm that ChatGPT/Codex subscription OAuth through this
third-party proxy is acceptable. Record the pinned version, checksum and MIT
license. Bind it explicitly to `127.0.0.1`; its default must never expose the
management or model API to the LAN or tailnet.

Spike the full intended path without changing ccmux:

1. Log in with Codex device OAuth and verify one forced token refresh plus three
   process restarts without another login.
2. Send an Anthropic Messages request naming a Claude-visible model, map it to
   the chosen OpenAI model, and receive the expected Anthropic response and
   effective model on ten consecutive requests.
3. Exercise streaming text, parallel tool calls and results, reasoning,
   cancellation, images, non-streaming, token counting and model listing; every
   case must match the Anthropic contract and leave the next turn usable.
4. Exercise bad auth, exhausted quota, process death and restart; each must
   return the expected status and account name without hanging or replaying.
5. Measure startup time and idle memory for one, five and ten account processes,
   record the numbers, and agree the host budget before integration.
6. Confirm OAuth files are owner-only and no token appears in logs, process
   arguments, environment exposed to panes, or an API response.
7. On every intended release OS and architecture, verify a pinned adapter
   artifact exists, its checksum and license can be packaged, and it starts from
   the same service environment and install path ccmux will use. A Mac must run
   this check before production integration; signing and notarization must also
   accept the chosen packaging shape.

Every numbered check is blocking. Hard failures also include any token leak,
cross-account request, duplicate dispatch, broken tool round trip, invalid
stream ordering, unsafe file permissions or inability to refresh OAuth. Add or
explicitly reject `/api/hello`, which CLIProxyAPI does not currently serve, and
test that decision before the spike can pass.

### Phase B: account kind and proxy contract (60-90 minutes)

Add an `openai-subscription` account kind. It is a Messages-surface account,
not the existing `codex` kind, because opencode remains on its stable Anthropic
provider while the sidecar translates to native OpenAI Responses.

Touch `internal/llmproxy` and `internal/harness` to add validation, dialect,
loopback URL defaults, auth stripping, compatible paths, system-turn handling,
subscription ordering and cross-kind port collision checks. Keep aliases in
ccmux so each selected account maps the requested logical tier before the
request reaches its sidecar. Define deterministic priority between Meridian,
OpenAI subscriptions and metered API accounts; an explicit harness order must
always win.

Tests must pin kind validation, route filtering, requested-to-effective model
mapping, per-account failover aliases, auth isolation, quota status and every
accepted Messages path. Route only to a sidecar that was ready before dispatch.
A connection failure after dispatch fails that request loudly rather than
replaying it: absence of a response does not prove the upstream did no work.
Existing status-based quota failover remains allowed only before any response
body is delivered to opencode.

Bind each request to one account and sidecar generation for its full lifetime.
Changing a pane route affects the next request only. Account edit or deletion
first removes that generation from new routing. An ordinary edit drains for at
most 30 seconds, then cancels any hung request and replaces the process. Delete,
disconnect or credential revocation is a security action: cancel in-flight
requests and stop the old process immediately rather than preserving access.

### Phase C: sidecar lifecycle and OAuth API (60-90 minutes)

Add `internal/openaisubscription` with a Meridian-style supervisor: one process
group per account, bounded restart backoff, synchronous stop, readiness probe
and status that separates process running, port ready, authenticated and quota
state. Generalize the API's current Meridian-only sidecar interface so account
save/delete reconciles both supervisors without mixing their credentials.

Add daemon endpoints to begin device login, report its URL/code and state,
cancel it, disconnect an account and read adapter status. CLIProxyAPI owns the
OAuth token files; ccmux stores only account metadata and the random local API
key. Settings save may persist before a process becomes ready, so the account
must not enter a live route until readiness and auth both pass.

Protect every OAuth endpoint with the daemon's existing authenticated settings
boundary. Bind each login attempt to one user, host and account; give it a short
expiry; permit only its owner to poll or cancel it; allow only one live attempt
per account; redact tokens, local API keys and callback details from responses
and logs. Create auth directories as `0700` and files as `0600`, then test
unauthorized poll/cancel, expiry, replacement and daemon restart.

### Phase D: both lenses (45-60 minutes)

Ship the same account kind, Connect/Disconnect flow, device-code state, model
aliases, readiness, quota and error text in the Mac and web lenses together.
Show requested logical tier, selected account and effective upstream model so
seamless routing remains visible when debugging. Make the Mac pane route picker
filter incompatible accounts like the web route endpoint already does.

Likely consumers are `DaemonModels.swift`, `DaemonSettingsView.swift`,
`AccountEditorView.swift`, `RemoteSessionService.swift`, `daemon/web/app.js`
and the settings and route handlers under `internal/api`.

### Phase E: pinned runtime and operations (30-45 minutes)

Choose one release strategy after the spike measures the binary: either bundle
the pinned CLIProxyAPI binary and license in ccmux archives, or install and
verify it as an explicit host prerequisite. Do not auto-update it separately
from ccmux. Update install, upgrade, GoReleaser, service PATH and runbook paths
only if bundling wins. Add health and version output that makes a mismatched or
missing adapter clear.

### Phase F: provider-neutral qa and solo-workflow port (75-120 minutes)

Port the Claude plugin content into opencode commands, skills and restricted
reviewer subagents. Remove every explicit vendor/provider model so subagents
inherit the session's logical tier. Replace Claude-only namespaced skill calls,
agent names and hooks with opencode equivalents. Keep `.claude/qa-gates`, the
signed receipt format and repository review settings unchanged.

Start with check, review, commit, status, push and ship. Add browser smoke and
automatic post-commit review only after the core workflow is green. Push and
ship must block on the receipt at the actual Git boundary even though opencode
has no exact Claude Stop-hook equivalent. Have qa setup install a repository
`pre-push` hook that validates the outgoing commits and current tree against the
signed receipt. Never overwrite an unmanaged existing hook: chain through a
managed dispatcher or fail setup with a clear instruction. Add an opencode
`tool.execute.before` check as defense in depth, including denial of
`--no-verify`, but do not call that the boundary. Test direct and scripted
pushes, tag pushes, workflow pushes, stale receipts, invalid signatures,
existing-hook chaining and the documented human `--no-verify` escape hatch.

### Phase G: live switch acceptance (30-45 minutes)

In one unchanged opencode conversation, run normal turns and qa reviewer
subagents on Claude, switch the pane to OpenAI, continue with tools, then switch
back to Claude. No pane or session restart is allowed. Confirm the next request
uses the new account, an in-flight request stays on its original account,
history remains valid, and status in both lenses names the effective model.

Repeat with quota exhaustion and one sidecar crash. Release only if fallback is
deterministic, no tool call is duplicated and each failure says which account
or adapter failed.

## Blast radius and verification limits

Consumers of the shared account contract include settings GET/PUT, harness
defaults and saved overrides, per-pane route menus, agent account pins, proxy
failover, model listing, sidecar status and both lenses. Unrelated-looking risks
include old harnesses becoming default-inheriting, stale sidecars remaining
first in an order, an old key surviving an account-kind change, and service PATH
differences between systemd and launchd.

Linux can cover Go tests, web syntax and real OpenAI OAuth/proxy behavior, but
not the Swift build, Swift tests, macOS launchd, signing or notarization. The
release tag's macOS job must close the build gap, and a Mac must exercise the
native Connect flow before release. Unit tests cannot prove subscription terms,
real quota behavior or token refresh; those remain live spike requirements.

## Phase A spike results (run 2026-09-10)

**Verdict: fail. Do not start Phase B or add CLIProxyAPI to production.**

The release artifact passed the core Messages translation cases but initially
failed owner-only credential creation and token-count consistency. An isolated
source build and a safer sidecar configuration close both failures, and actual
OpenCode consumption now passes. Phase A still blocks production integration
on live exhausted-quota and cross-account behavior, pane-level diagnostics and
isolation, an accepted host budget, the intended service/install environment,
and macOS execution/signing/notarization.

### Pinned candidate and host

| Item | Exact result |
|---|---|
| Release | CLIProxyAPI `v7.2.156`, published 2026-09-10 02:41:41 UTC |
| Commit | `d1a024e9400bc65bd78ccd908945cf2eacc2835e`; the `v7.2.156` tag resolves directly to this commit |
| Binary self-report | `CLIProxyAPI Version: 7.2.156, Commit: d1a024e9, BuiltAt: 2026-09-10T02:42:15Z` |
| License | MIT; packaged `LICENSE` SHA-256 `879792e89cf1bdd6a8d446033ec87e30496f97dcafc4656dc53f641509b346a6` |
| Downloaded archive | `CLIProxyAPI_7.2.156_linux_amd64.tar.gz`, SHA-256 `2bf0515e70be3255be4bd13edb352febe7a206c2b823a6b49301d6f37ef40c85`; upstream manifest check passed |
| Extracted binary | SHA-256 `b3c38219b780233893ac049cd8e40398851448259c285e34adb305809007c35a` |
| Source snapshot | Commit tarball SHA-256 `8a6af4345af28e7056536023038cf3dbc68df8c87745564dcd326991c7fb95ef` |
| Remediation build | Same pinned source plus the two changes recorded below; binary SHA-256 `0cce47348b69ced67dc1816835f3b55ba978aa52d8f4665197eb05602e076b92` |
| Host | `Linux 6.8.0-137-generic #137-Ubuntu SMP PREEMPT_DYNAMIC Fri Jul 17 20:28:23 UTC 2026 x86_64 GNU/Linux` |
| Test clients | Bun `1.3.14`; curl `8.5.0-2ubuntu10.13`; ImageMagick `6.9.12-98` |

The upstream checksum manifest also contains every ccmux release target:

| ccmux target | Pinned CLIProxyAPI asset | SHA-256 | Executed here |
|---|---|---|---|
| darwin/arm64 | `CLIProxyAPI_7.2.156_darwin_aarch64.tar.gz` | `c41f985edc744a70dc962ec4250889bad02bdd9be816855021d4d0d93ddf4c92` | No |
| linux/amd64 | `CLIProxyAPI_7.2.156_linux_amd64.tar.gz` | `2bf0515e70be3255be4bd13edb352febe7a206c2b823a6b49301d6f37ef40c85` | Yes |
| linux/arm64 | `CLIProxyAPI_7.2.156_linux_aarch64.tar.gz` | `c570113560838a1c401b566f34ea6f1fa1c00b7699e515fe1a3e82713459b2b2` | No |

Artifact existence is only the availability part of gate 7. No Mac ran the
binary, launchd environment or intended install path, and no signing or
notarization check accepted this packaging shape. The Linux binary ran from
`/tmp`, not from a future ccmux installation path.

### Exact setup and process commands

Secrets below were random 32-byte values and are intentionally redacted. They
were stored only in the `0600` config, never in an argument or environment
variable.

```sh
install -d -m 700 \
  /tmp/ccmux-cliproxyapi-v7.2.156 \
  /tmp/ccmux-cliproxyapi-v7.2.156/auth \
  /tmp/ccmux-cliproxyapi-v7.2.156/config \
  /tmp/ccmux-cliproxyapi-v7.2.156/bin \
  /tmp/ccmux-cliproxyapi-v7.2.156/logs
curl --fail --location --silent --show-error \
  --output /tmp/ccmux-cliproxyapi-v7.2.156/CLIProxyAPI_7.2.156_linux_amd64.tar.gz \
  https://github.com/router-for-me/CLIProxyAPI/releases/download/v7.2.156/CLIProxyAPI_7.2.156_linux_amd64.tar.gz
curl --fail --location --silent --show-error \
  --output /tmp/ccmux-cliproxyapi-v7.2.156/checksums.txt \
  https://github.com/router-for-me/CLIProxyAPI/releases/download/v7.2.156/checksums.txt
cd /tmp/ccmux-cliproxyapi-v7.2.156
sha256sum --check --ignore-missing checksums.txt
tar -xzf CLIProxyAPI_7.2.156_linux_amd64.tar.gz -C bin
./bin/cli-proxy-api --help
./bin/cli-proxy-api \
  -config /tmp/ccmux-cliproxyapi-v7.2.156/config/config.yaml \
  -local-model
```

The isolated config set `host: "127.0.0.1"`, port `18317`, the absolute
isolated auth directory, one random local API key, TLS off, management off,
pprof off, plugins off, file logging on, usage aggregation off,
`request-retry: 0`, `max-retry-credentials: 1`, retry wait zero, stream
bootstrap retries zero and WebSocket authentication on. It mapped `quality`
to `gpt-5.6-terra`, `balanced` and `claude-sonnet-4-6` to `gpt-5.6-sol`, and
`fast` to `gpt-5.6-luna`.

Listener checks:

```sh
ss -ltnp 'sport = :18317'
curl --noproxy '*' --silent --show-error --max-time 2 \
  http://192.168.1.232:18317/v1/models
curl --noproxy '*' --silent --show-error --max-time 2 \
  http://100.99.239.55:18317/v1/models
```

`ss` showed only `127.0.0.1:18317`. Both LAN and tailnet addresses refused the
connection immediately. An unauthenticated loopback model request returned
401; the random key returned 200.

After explicit user approval, device login used:

```sh
./bin/cli-proxy-api \
  -config /tmp/ccmux-cliproxyapi-v7.2.156/config/config.yaml \
  -codex-device-login -no-browser -local-model
```

It contacted `https://auth.openai.com/codex/device`, completed once, and wrote
one Codex credential. The device code, account email, OAuth tokens and all
local keys are omitted here.

### OAuth and secret handling

- The device implementation talks only to `auth.openai.com`, uses the Codex
  public client ID, obtains an authorization code and PKCE values through the
  device flow, then exchanges at the OpenAI token endpoint. Access, refresh and
  ID tokens are stored as plaintext JSON.
- Project decision after review: CLIProxyAPI's Codex device OAuth mechanism is
  acceptable under the applicable OpenAI terms. The proxy already implements
  that mechanism, and the user explicitly approved using it for this spike.
  The OAuth policy gate passes. CLIProxyAPI defaults
  `disable-codex-cloaking` to false, so the test exercised its normal official
  Codex header behavior rather than a special spike configuration.
- **Release artifact credential permission failure:** the first OAuth JSON was
  created as `0664` under this host's service umask because
  `internal/auth/codex/token.go` used `os.Create`. The remediation build changes
  `CodexTokenStorage.SaveTokenToFile` to enforce `0700` on the auth directory,
  open the token with `0600`, and apply `f.Chmod(0600)` when replacing an
  existing file. Its regression test starts with a `0755` directory and `0664`
  token, saves, and observes `0700`/`0600`. A fresh live device login with the
  patched binary also created the directory and OAuth JSON as `0700`/`0600`
  without a corrective `chmod`.
- A forced refresh was tested by temporarily enabling loopback management with
  a second random key and calling
  `POST /v0/management/auth-files/refresh?all=true`. It returned 200 in 669 ms;
  SHA-256 fingerprints of both access and refresh tokens changed, and expiry
  and `last_refresh` advanced without another login. Management was then
  disabled and returned 404. Merely backdating `expired`, then both `expired`
  and `last_refresh`, did not cause a refresh within 45 seconds, 30 seconds
  after one restart, or 35 seconds after another restart. Because the real JWT
  was still valid, that does not disprove refresh at real expiry; real
  time-based expiry was not tested.
- Three explicit post-login restarts needed no login. The verification turns
  returned `RESTART_1_OK`, `RESTART_2_OK` and `RESTART_3_OK` in 3,711 ms,
  1,720 ms and 1,342 ms.
- The final leak scan compared 11 distinct current and rotated OAuth
  access/refresh/ID-token values plus the local and temporary management keys
  against `main.log`, process stdout, OAuth stdout, `/proc/<pid>/cmdline`,
  `/proc/<pid>/environ`, an API response and the config excluding its expected
  local-key slot. It found zero matches. The isolated auth directory, final
  auth file and config ended as `0700`, `0600` and `0600`. No ccmux pane was
  involved, so pane-environment leakage was not exercisable.
- Logs did expose the account email as part of the generated auth filename.
  No OAuth token or local key appeared.

### Messages-surface results

Requests were sent directly with Bun `fetch` to `127.0.0.1:18317`. Common
headers were `content-type: application/json`,
`anthropic-version: 2023-06-01` and
`x-api-key: [random-local-key]`.

| Case and exact request shape | Result |
|---|---|
| Non-streaming `POST /v1/messages`, model `claude-sonnet-4-6`, `max_tokens: 64`, one user text asking for `TRANSLATION_OK` | 200 in 5,137 ms; Anthropic message shape, effective model `gpt-5.6-sol`, `end_turn`, exact text |
| Streaming Messages, model `balanced`, `max_tokens: 64`, `stream: true` | 200 in 1,549 ms; `text/event-stream`; exact event order `message_start`, `content_block_start`, two `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`; effective model `gpt-5.6-sol`; exact `STREAM_OK` |
| Parallel tools `lookup_alpha({"key":"alpha"})` and `lookup_beta({"key":"beta"})` | First response 200 with two unique tool-use IDs and `stop_reason: tool_use`; one follow-up supplied both tool results and returned 200, `TOOLS_OK`, `end_turn` |
| Reasoning, model `quality`, `thinking: {type:"enabled", budget_tokens:1024}`, `max_tokens: 2048` | 200 in 1,535 ms; effective `gpt-5.6-terra`; a `thinking` block with an opaque signature and empty plaintext, 12 thinking tokens, final `323`; replaying the block in the next turn returned `REASONING_OK` |
| Client cancellation of a 4,096-token stream | First 285 bytes in 602 ms, then client abort raised `AbortError`; server log duration was also 602 ms; next turn returned `AFTER_CANCEL_OK` in 3,072 ms. Upstream cancellation had no independent trace, so only downstream and process behavior were observed |
| Logical aliases `quality`, `balanced`, `fast` | All 200; effective models respectively `gpt-5.6-terra`, `gpt-5.6-sol`, `gpt-5.6-luna`; exact expected texts; 1,502/1,946/1,682 ms |
| Ten consecutive non-streaming requests naming Claude-visible `claude-sonnet-4-6` | 10/10 returned 200, effective `gpt-5.6-sol` and exact numbered text; latencies 2,996, 1,847, 2,741, 3,381, 1,495, 2,745, 2,021, 2,249, 2,181 and 1,773 ms |
| Image input: generated with `convert -size 32x32 xc:red /tmp/ccmux-cliproxyapi-v7.2.156/red.png`, then sent as an Anthropic base64 PNG block | 200, effective `gpt-5.6-sol`, identified `RED`; next turn returned `AFTER_IMAGE_OK`. An earlier copied 1-pixel fixture was rejected 400 as invalid image; replacing the fixture proved that was test data, not the adapter |
| Explicit `stream: false`, model `fast` | 200 `application/json`, not SSE; effective `gpt-5.6-luna`, exact `NONSTREAM_OK`, `end_turn` |
| Authenticated `GET /v1/models` | 200 with 13 models. It listed `quality`, `balanced`, `fast`, `claude-sonnet-4-6`, `codex-auto-review`, `gpt-5.3-codex-spark`, `gpt-5.5`, `gpt-6-astra` and five `gpt-image-*` models. The aliased Terra/Sol/Luna originals were hidden |
| `GET /api/hello`, with and without the local key | 404 both times. Decision: explicitly reject this path and use authenticated `GET /v1/models` as the readiness probe; that probe returned 200 |
| `POST /v1/messages/count_tokens` versus the identical generated request | **Remediated pass:** the release/default configuration was 306 tokens apart because automatic `image_generation` injection adds 296 opaque upstream input tokens. With `disable-image-generation: "chat"`, six plain, long, history, system and one/two-tool cases differed by -4 to +15 tokens; the 262-token long case counted 268 upstream, a 2.3% difference. This is accepted as non-material estimator overhead |

OpenCode `1.18.30` was then run with isolated HOME/XDG state, `--pure`, an
inline custom `@ai-sdk/anthropic` provider and model `ccmux-spike/balanced`.
`opencode run --format json` returned exact text `OPENCODE_OK`; its terminal
event reported 6,304 input and 8 output tokens. No global or repository
OpenCode configuration was changed.

### Remediation pass

The second pass rebuilt the exact pinned source rather than changing ccmux.
Besides the credential-mode fix, `CodexExecutor.CountTokens` now applies the
same original-request selection, payload configuration, image-tool policy,
parallel-tool normalization and tool-schema normalization as generation.
Focused Go tests passed before building the binary.

The decisive count discrepancy was configuration, not prompt tokenization.
With automatic image generation enabled, plain/long/tool cases counted
5/262/43 locally but reported 307/564/348 upstream: deltas 302/302/305.
Disabling automatic injection reduced those same deltas to 6/6/9. The final
spike configuration uses `disable-image-generation: "chat"` so Messages does
not silently inject the native image tool while the explicit image endpoints
remain available. A 32x32 red Anthropic image input still returned exact text
`RED` through `gpt-5.6-sol`.

The post-remediation leak scan compared all three live OAuth token values
against the sidecar logs and isolated OpenCode home, config, data, runtime and
project state. It found zero OAuth-token matches and zero local-key matches.

An isolated front-proxy prototype also attached account
`quality-openai` to raw quota and transport errors. A simulated exhausted
upstream returned the original 429 plus `quota_exhausted` and
`weekly limit reached`; a stopped upstream returned account-named 502 in 1 ms.
The upstream dispatch counter stayed at one. This proves the intended thin
ccmux layer can add account context and avoid replay; it does not substitute
for the still-required live exhausted-account, cross-account and pane tests.
Neither remediation patch nor the prototype was added to ccmux production
code.

### Failure and restart behavior

| Case | Exact result |
|---|---|
| Wrong local API key | 401 in 1 ms, `{"error":"Invalid API key"}`, no upstream dispatch |
| Kill sidecar after the first chunk | Initial 200; first 285 bytes at 2,314 ms; socket-close error observed at 2,375 ms; no hang. Restart reached authenticated model readiness in 258 ms and a new request returned `AFTER_DEATH_OK` in 1,717 ms |
| Invalid access token with valid refresh token | Adapter transparently refreshed and returned 200 in 3,576 ms. This proves recoverable auth, but the client is not told which account refreshed |
| Invalid access and refresh tokens | 401 in 7,004 ms with Anthropic `authentication_error`; no account name or adapter identity in the body. Restoring the saved credential and restarting recovered in 309 ms |
| Exhausted subscription quota | **Not tested:** the authorized account was not exhausted, and the spike did not deliberately consume its remaining quota |

The raw sidecar's process-death socket error and irrecoverable-auth body still
do not identify a ccmux account because CLIProxyAPI does not know that ccmux
identity. The remediation prototype proves the ccmux-facing layer can add the
name, preserve raw detail and avoid replay for pre-body quota/transport
failures. The actual pane path, a mid-stream labelled failure, an exhausted
subscription and two-account isolation remain unproven.

One useful readiness failure appeared during the scale setup. Port 18318 was
already occupied by an unrelated loopback Python fixture. CLIProxyAPI printed
`API server started successfully on: 127.0.0.1:18318` before reporting its bind
error and exiting. A log match plus an open port can therefore accept the wrong
process. A future supervisor would have to require the per-account random key
to succeed on an authenticated semantic probe and confirm its expected model
aliases; PID existence plus port reachability is insufficient.

### Startup and resource measurements

All scale processes used the same extracted binary, isolated config directory
and isolated auth directory, distinct loopback ports, file logging off and the
same current credential. This measures process cost, not cross-account
isolation.

| Processes | Authenticated readiness | Idle RSS |
|---|---|---|
| 1 | 219 ms | 47,104 KiB |
| 5 | 221 ms for the concurrent batch; individual 172-221 ms | 223,232 KiB total |
| 10 | 222 ms for the concurrent batch; individual 175-222 ms | 445,440 KiB total |

One active long-stream sample raised the main process from 47,104 KiB to
56,832 KiB RSS. Idle CPU after startup was 0.0-0.5% per process. Reported VSZ
was about 3.1 GiB per Go process and is not resident memory. A conservative
budget if this candidate is reconsidered is 64 MiB idle and 80 MiB active per
account process: 640/800 MiB for ten. That budget has not been accepted for
production.

### Blocking disposition

Pass: OAuth policy acceptance, pinned reproducible Linux artifact, MIT license,
explicit loopback bind, local API authentication, forced refresh, restart
persistence, translation, stream ordering, parallel tool round trip, opaque
reasoning round trip, downstream cancellation, aliases, ten-request run, image
input, non-streaming, token counting with automatic image injection disabled,
model listing, `/api/hello` rejection, owner-only credentials in the
remediation build, actual OpenCode consumption and no observed token leakage.

The account-labelled failure prototype passes in isolation. Remaining blocking
gates: live exhausted subscription quota, mid-stream pane diagnostics,
duplicate-dispatch proof on the actual path, live cross-account isolation,
pane-environment leakage, acceptance and packaging of the two downstream
patches, accepted host budget, Linux arm64 execution, macOS execution, launchd
path/environment, signing and notarization.

Phase A therefore still fails, but the observed Linux/amd64 protocol defects
are now closed by small, explicit changes. Keep Meridian as the only production
subscription adapter until the remaining live, platform and packaging gates
pass.

State left on the host: all spike, scale and remediation sidecars were stopped,
then `/tmp/ccmux-cliproxyapi-v7.2.156` and
`/tmp/ccmux-cliproxyapi-v7.2.156-remediation` were removed, including OAuth
credentials, random keys, logs, patched source/binary, OpenCode state and
fixtures. The pre-existing unrelated `cliproxy-mock` process was not touched by
the spike. No ccmux code, settings, account or pane was changed; this document
is the only repository file touched.
