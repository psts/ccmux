# Agent specification: what to decide when you create an agent

Status: draft v1, 2026-09-05. Companion to `agents-plan.md`. Read this before
creating a new base agent, and again before adding one to a project.

An agent is a **base** (a folder of plain files, shared by every project) plus
zero or more **instances** (the base added to one project). The base says what
the agent is. The instance says what it is for here.

A project is a **shared window**, not a repo (changed 2026-09-06). An instance
is its own session inside the window, named after the agent, and every
session in that window reaches it on the bus by name. The bus tells it where
the project is: the window's repo sessions (name, folder, live or archived)
and its agents reach every session as one paragraph, rendered by
`internal/buscontext`. Claude Code gets it as the peers shim's MCP
instructions and as a `list_peers` footer; opencode ignores MCP
instructions, so the ccmux opencode plugin fetches the same text from
`GET /v1/panes/{id}/bus-context` at every turn and appends it to the system
prompt. So an agent reads the code at those folders or messages the session
by name, and nobody writes repo paths into its instructions. A claude agent additionally
gets each folder as `--add-dir` (its base's permissions say whether it may
edit there, and the defaults allow edits). Its folder lives beside the bases,
not inside any repo. The folder name is the window's slug plus the first
eight characters of its id, so two windows whose names slug alike stay apart.

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
- Skills arrive from where they already live, never from a text box: the
  agent editor takes a GitHub folder URL (or any git URL plus a path) and
  does a shallow sparse clone of that folder into `skills/`, keeping the
  source in `.source` so it can be updated; or files dropped on it. Routes:
  `GET/POST /v1/agents/{name}/skills`, `GET|DELETE
  /v1/agents/{name}/skills/{skill}`, `POST .../skills/{skill}/update`.
  opencode reads the base's `skills/` through its config (`skills.paths`),
  Claude Code through `--plugin-dir`.

## 4. Tools and MCP servers

- List every MCP server the agent needs in `mcp.json`, neutral shape:
  `{ "<name>": { "command": "...", "args": [...], "env": {...} } }`.
  Secrets come from env names, never literal values.
- `ccmux-peers` is always present; do not list it.
- Servers arrive as the snippet a server's readme ships, pasted into the
  agent editor: `{ "mcpServers": { ... } }` or the bare map, merged by name
  (`GET/POST /v1/agents/{name}/mcp`, `DELETE /v1/agents/{name}/mcp/{server}`).
- opencode plugins (npm names or paths) are `agent.json.plugins`, loaded
  beside ccmux's own; other harnesses ignore them.
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
neutral vocabulary `read | edit | bash | webfetch` → `allow | ask | deny`,
plus a list of bash patterns. The adapter translates it: opencode gets it as
its `permission` block in `opencode.jsonc`; Claude Code gets `--allowedTools`
for allow (and each bash pattern as `Bash(pattern)`) and `--disallowedTools`
for deny, with ask left to its default prompt. `memory` and `start` are
recorded for the UI and not yet acted on.

Defaults: `read: allow`, `edit: allow` inside the instance folder and
`--add-dir` folders, `bash: ask`, `webfetch: ask`. An agent that posts to the
outside world (X, email, a CMS) gets `ask` on that tool until it has run
clean for a week, then `allow` with a bash pattern list.

## 7. Memory policy

Agents write their own memory; no layer is human-curated. Three layers.

| Layer | Owner | Lives | Example |
|---|---|---|---|
| Project instructions | whoever added the agent | instance `AGENTS.md`, `.claude/skills/` | project voice, who approves |
| Agent memory | the agent | instance `memory/` and `log.md` | "posts with images get 3x replies here", "last thread 2026-09-05" |
| Window shared | every agent in the window | `shared/` beside the window's `agents/` (created, with a README, by the first agent start there) | what one agent learned that another in the same project needs |

The bus names the shared folder to every session in the window (same
paragraph as the repo sessions), and a claude agent gets it as one more
`--add-dir`. Its layout is a convention, seeded by the README the daemon
writes there once (2026-09-08): each agent owns `shared/<its name>/` and
writes nowhere else; one dated file per finding (`YYYY-MM-DD-<slug>.md`),
never rewritten; frontmatter `date`, `agent`, `topic`, `source`, `tags` so
grep finds things across agents; a `README.md` per agent folder saying what
it publishes; and the bus for questions the files do not answer. No database
and no search tool until a folder outgrows grep. There is no base `knowledge/` folder (dropped 2026-09-07):
learning that crosses windows has no home yet. Memory is per instance. `agent.json.memory:
"shared"` is recorded but NOT implemented yet: every instance still gets its
own folder. `start: "continue"` is the default for whether a wake resumes the
instance's last opencode session (the chat view offers the choice per wake);
`sideEffects` is stored and shown, not acted on. Enforcement is a follow-up.

## 8. Lifecycle

| Decide | Default | When to change |
|---|---|---|
| Start mode | fresh conversation per start | `continue` for one-long-thread agents |
| Idle exit | 10 minutes with no open task | shorter for chatty cheap agents, longer for slow external APIs; 0 = never |
| Keep alive | off | on for agents that must answer inside a second |
| Concurrency | daemon cap shared by all agents | pin `maxConcurrent: 1` for agents with a rate-limited upstream |
| Autostart | off | on only with keep alive |

Implemented (2026-09-05): the daemon's lifecycle loop ticks every 30 s.
Busy/idle comes from the harness: Claude Code panes through their hooks,
opencode panes through the embedded ccmux plugin (`agents/.ccmux/
ccmux-opencode.ts`, listed in every instance's `opencode.jsonc`), which posts
to `POST /v1/panes/{id}/agent-signal` on loopback. An instance idle past
`idleExitMinutes` (0 = never) with no open peer delegation gets ctrl-d and
drops to its shell ("asleep") — note the loop cannot tell a human composing
in the TUI from an idle agent, so a draft in a running agent's input can be
lost at the cap; a keep-alive instance found asleep is started again from
the current base, and a keep-alive instance whose base version moved exits
when idle so that restart picks the new base up. `-agents-max` (default 6)
caps running instances; asleep ones do not count.

**Schedules (2026-09-08).** An agent can ask for timed runs of itself from
its own chat: the peers shim's `schedule` tool (both harnesses load the
shim) turns "every Monday at 07:00" into a five-field cron line plus the
prompt to deliver, and posts it to `POST /v1/panes/{id}/schedules`
(`action: add|list|pause|resume|remove`). The daemon keeps one row per
schedule in `agent_schedules` (window + agent + cron + prompt + next run)
and answers with the next run time, which the agent repeats to the human.
A 30 s tick in the api layer (`agentschedules_fire.go`) delivers a due
prompt the way a chat message would: pushed into a running, idle opencode
TUI, or as the first line of a start when the instance is asleep or not
yet in the window. Busy waits for the next tick; a slot missed by more than
six hours (daemon down, agent busy) skips ahead; a running Claude-harness
instance has no chat path and is skipped with a log line. The lenses list,
pause and remove schedules from the agent chat header (the same pane route)
and show a count per instance in the editor
(`GET /v1/windows/{id}/agents/{name}/schedules`, read-only).
Cron parsing is `internal/schedule`, hand-rolled; the host's local zone.

## 9. Bus contract

- Discovery text = description. Other agents see: name, description, status.
- Contact by name starts it. Design the first message handling so that a
  cold start with a delegation task lands correctly: acknowledge, work, close.
- Replies always go to `from_id`.
- If the agent delegates, it uses `delegate`, not `send_message`, and it
  closes the loop on task updates.
- Permission relay: the agent answers `yes|no <id>` only for work it asked for.

Implemented (2026-09-05, window-keyed since 2026-09-06): `POST
/v1/peers/agents` gives a session the bases with their state in its own
window, `POST /v1/peers/sessions` (2026-09-07) its repo sessions; the shim
appends a PROJECT SESSIONS paragraph and an AGENTS ON THIS BUS paragraph to
the instructions and the matching footers to `list_peers`, and `GET
/v1/panes/{id}/bus-context` serves the same paragraph to the opencode
plugin.

## 10. The chat view (2026-09-07)

An agent pane shows its conversation as a chat in both lenses, the raw
TUI behind a per-pane "Terminal" toggle. The daemon is the one reader of
opencode's server: `GET /v1/panes/{id}/agent/ws` (and a one-shot `GET
/v1/panes/{id}/agent`) sends a hello with the agent's state (asleep,
starting, running), its newest session in the instance folder, the
transcript and the permission requests waiting, then turn, part, delta,
idle, permission, question and error frames as opencode's event stream
reports them; the lens sends prompt, abort, permission replies (once,
always, reject) and question answers (the chosen labels per question, or a
reject). Asleep, the transcript comes from opencode's store through its
CLI (`session list`, `export`), and a prompt wakes the agent with that
text. The transcript is the agent's last four conversations in one scroll,
oldest first, each behind a session marker (a role "session" turn), so a
woken agent's earlier work stays above the fresh conversation. Asleep, the
prompt box offers "Continue previous conversation": on, the launch adds
`--session <newest>` on the typed line (never the persisted one); the
base's `start` is the default, and bus wakes follow it. One normalized shape, `agent.Turn`, in `internal/agent/transcript.go`;
`daemon/web/agentchat.js` and `Sources/ccmux/Views/AgentChatPaneView.swift`
render it. Claude-harness agents keep the terminal for now. `send_message(to_name=<agent>,
spawn_if_missing=true)` from a pane starts or wakes the agent in the SENDER's
window (its bus group) and delivers the message once it registers. A sender
outside a shared window is told the agent needs one. opencode instances receive bus messages typed into their TUI
through the instance's server (channel pushes do not reach opencode); Claude
Code instances get them as channel messages.

## 10. Cost and safety caps

- `maxTurnsPerTask` (default 40) and `maxTokensPerTask` (default 400k) in
  `agent.json`. Recorded and shown; NOT enforced yet — the daemon has no
  per-session token meter. Enforcement is a follow-up; until then the idle
  cap and the concurrency cap are the only hard limits.
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
- Which repo folders does it need beyond the window's own? `addDirs`. The
  window's repos and its `shared/` folder come for free.
- Which secrets? `KEY=VALUE` lines in the instance `.env`, or in the
  window's `shared/.env` when every agent there needs them.
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
└── mcp.json
```

### Window folder

```
~/.ccmux/windows/<window-slug>-<id prefix>/
├── shared/            every agent in the window reads and writes it
│   ├── README.md      written once
│   └── .env           optional; loaded into every agent's environment at start
└── agents/<name>/     one instance folder per agent added to the window
```

The folder is found by lookup, so a renamed window keeps it: a folder
ending in the window's id prefix, else a new one. Never by name alone.

### Instance folder

```
~/.ccmux/windows/<window-slug>-<id prefix>/agents/x-poster/
├── AGENTS.md          starter header + project instructions
├── CLAUDE.md          @AGENTS.md
├── .claude/skills/
├── memory/MEMORY.md
├── log.md
├── .env               optional; this agent's own secrets, wins over shared/.env
└── opencode.jsonc     GENERATED at every start from the base: instructions
                       path, permissions, MCP servers + peers bus, snapshot off.
                       The one daemon-owned file here; do not edit.
```

### Secrets: `.env`

Never in memory, notes or instructions. One `KEY=VALUE` per line, the
value taken as-is (surrounding quotes stripped, no shell syntax), in
`shared/.env` (every agent in the window) or the instance's `.env` (this
agent only, wins). The launch line is `ccmuxd env-exec -f shared/.env -f
<instance>/.env -- <harness line>`: the verb parses the files as data and
execs the harness with the variables set, so nothing in a file ever runs
(an agent that can write a file must not gain a shell past its permission
gate), the values ride in the harness's environment and never in the
persisted command, and the files are read again at every start, so a token
an agent saved is back next time. An agent that obtains a credential
writes it there itself.

Implemented in `daemon/internal/agent` (2026-09-05): `Store` over
`~/.ccmux/agents` (`-agents-dir`), `Reject`, `Save` (bumps the patch version
when agent.json or AGENTS.md changed, appends CHANGELOG.md, never touches
skills/mcp.json), `Delete`, `Bootstrap` (instance folder, once),
`WriteInstanceConfig`. API: `GET /v1/agents`, `PUT /v1/agents/{name}`
(per-name upsert, not list replace), `DELETE /v1/agents/{name}`.

Instances (2026-09-06): `GET /v1/windows/{id}/agents` lists every base with
its state in that window (absent, asleep, running) and the id of its session
and pane; `POST /v1/windows/{id}/agents/{name}` `{prompt, createdBy}` adds
the agent as a new session in the window (201) or wakes its existing one
(200), the prompt as first message; `DELETE .../agents/{name}` puts it to
sleep (ctrl-d, history kept). Removing the session is the session's own
Remove. The agent session carries `agent: "<name>"` on the workspace and
`agent`/`agentVersion` on its first pane; its name is the base's icon and
name. The folder path is recorded on the session (its RepoPath) and never
rebuilt, so renaming a window keeps memory for agents already added; a fresh
add after a rename makes a new folder under the new slug. Agent sessions run
on the daemon lenses talk to (the hub); repos on other hosts are not passed
as extra directories. A closed window archives its agent sessions like any
other; opening it revives them with their persisted launch, so they come up
running and idle exit puts them back to sleep.

Upgrading from the per-repo model (0.1.47): an agent pane that sat inside an
ordinary project session becomes a plain pane on the daemon's first open
(the lifecycle stops waking it, the bus treats it as any other); its folder
under `<repo>/.ccmux/agents/` stays on disk and is no longer read. Add the
agent to the window to get it back.

Sleep (and idle exit) is a ctrl-d at the harness prompt. Verified 2026-09-06
on opencode: at its plain prompt it quits; with a permission dialog open the
keystroke rejects the dialog instead and the agent stays running, so a
second sleep is needed once the dialog is gone. Same known hole as a human
mid-typing; a state-aware exit is a follow-up.


## Lenses (2026-09-05)

Web (2026-09-07): Settings → Agents is a list; New and Edit open one modal
with a section per concern (identity, role, harness and model, permissions,
skills, MCP servers, opencode plugins, lifecycle, instances). Every skill
or server change bumps the base; the Instances section lists where the base
is deployed with the version each runs, and "Restart to apply" sleeps and
wakes the running ones (`GET /v1/agents/{name}/instances`, `POST
.../instances/restart`); asleep ones pick the base up on their own. The
Mac has the same editor as a sheet. Before that: rows save on change, the WINDOW
header's ⚙ button or right-click lists every base with its state here
(● running opens its session, ■ puts it to sleep, ○ asleep wakes it, + not
yet added adds it as a new session), a sleeping agent pane shows a composer
over its history (Enter wakes it with the text as first message, with a ↻
note when the base has moved), and agent panes carry a ⚙ tab mark. Session
menus no longer carry agent rows (2026-09-06).

Mac: the same Agents tab and the same rows on the window header's right-click
menu (open and sleep for running, wake or add with a first-message prompt box
for the rest); the chat view (§10) is the composer on both. The lists
refresh every poll for open windows, and agent pane tabs carry a gear on
both (⚙ in the web strip, gearshape in the Mac tab bar, 2026-09-07).
Unbuilt on the Linux host; the release tag job builds it.
