# ccmux

## One lens: the web lens

The native Mac app was removed from `main` on 2026-11-04 (user's call). Its
code (`Sources/`, `Tests/`, `SwiftTerm/`, `build-app.sh`, the release
workflow's Mac job) is kept, frozen, on the `mac-app` branch. The web lens
(`daemon/web/`) is the only lens: there is no parity rule and no Swift side to
update. Comments in the daemon and web code that mention the Mac app or
SwiftTerm describe history; they are not a to-do.

The daemon can still run on macOS (darwin builds in `.goreleaser.yaml`,
launchd in `ccmuxd install`). Only the app is gone.

## What a signed receipt covers

`.claude/qa-gates` is the list; read it there rather than trusting a copy here.
Gates that need a tool this host lacks FAIL rather
than skip, so a fresh clone needs `go install
github.com/fzipp/gocyclo/cmd/gocyclo@latest` before it can sign anything.

`daemon/web` has no build step, bundler or lint. `node --check` is a parse, not
a lint: it catches a typo, never a logic error.

The complexity cap is 10, and the gate is a ratchet over the debt that existed
when it landed (`.claude/gates/gocyclo-frozen.txt`, the non-test functions listed there).
Anything new over the cap fails, and so does a frozen function that gets worse.
**The frozen list is delete-only.** Split the function, or land it under the
cap; adding a line to keep a gate quiet is how the list stops meaning anything.

Pruning is not left to memory: the gate walks the list itself and prints
`PRUNE` for an entry that is gone or under the cap and `IMPROVED n -> m` for one
that got better while holding its old number. Those REPORT, they do not fail —
a refactor that improved something should not turn the gate red — so the list
still only shrinks when a human acts on that output.

## The LLM proxy is its own service (Linux)

Since task #173 the LLM routing proxy does not run inside ccmuxd on Linux. It
is the same binary run as `ccmuxd llm-proxy` (`daemon/internal/llmserve`),
under the systemd user unit `ccmux-llm-proxy.service`, on the port after the
daemon's (7901 beside a daemon on 7900). A pane's `ANTHROPIC_BASE_URL` names
it, so restarting ccmuxd no longer cuts a reply a session is streaming.

- **It restarts only when its own code or its unit file changed.** The daemon
  decides at its own start (`cmd/ccmuxd/llmproxyunit.go`), from what the running
  service reports on `GET /health`: its code (`llmserve.CodeHash()`, stamped by
  `build-daemon.sh` from `daemon/scripts/llm-proxy-hash.sh`) and the mark of the
  unit file it was started from (`-unit`).
- **Keep `internal/llmserve`'s imports small.** The hash covers that package
  and every package of this module it imports (today: `llmproxy`, `harness`, `meridian`).
  Import `store`, `manager` or `api` there and nearly every deploy restarts the
  proxy again. It reads the daemon's database itself, read only
  (`reader.go`); the daemon stays the only writer.
- **A change under `internal/llmproxy`, `internal/harness`,
  `internal/meridian` or `internal/llmserve` restarts the proxy on the next
  deploy**, after a 10s drain, and cuts whatever is still streaming then;
  the meridian sidecars restart with it. Claude Code retries a cut
  reply by itself (seen live 2026-11-19), so it costs tokens, not work.
- A pane keeps the address it started with. Panes from before the move still
  call `http://127.0.0.1:7900/llm/pane/<id>`; the daemon passes those on to the
  service, and they get the benefit at their next start.
- Its log is its own: `journalctl --user -u ccmux-llm-proxy` (the `llm: pane …`
  lines and the `meridian[<account>]` ones used to be in ccmuxd's). `GET
  /v1/health` on the daemon carries `llmProxy` (mode, url, serving, what the
  last daemon start did to it) and `llmSidecarsRunning` (account →
  running; `GET /v1/settings` has the rest as `llmSidecars`).
- **A scratch daemon never touches the live unit.** With `CCMUXD_ROOT` set it
  keeps the proxy in its own process unless it is given `-llm-proxy-unit
  <its own name>`; and any daemon that finds a unit file serving another
  database leaves it alone. A test that names a unit must disable and delete
  that unit file afterwards.
- macOS, and any host without systemd, keep the proxy inside the daemon
  (`-llm-proxy-unit none` forces that on Linux too).
- **The meridian sidecars run where the proxy runs** (task #187,
  `llmserve/sidecars.go`; `cmd/ccmuxd/sidecars.go` decides): in the service,
  so a deploy no longer restarts them and the pool no longer fails over
  (maybe to a metered key) while they come back. The service reads the
  meridian accounts itself every 5s; a settings apply on the daemon only asks
  it to look at once (`POST /ctl/sidecars/reconcile`). An unreadable database
  leaves the running ones alone. They stop after the drain, not with it
  (`KillMode=mixed`: the stop signal reaches the service alone).
  Where the proxy is in the daemon, so are they, as before. A daemon switched
  to `-llm-proxy-unit none` while the old unit still runs fights it for the
  sidecars' ports: stop that unit first.

## Long work runs as jobs (Linux)

Since task #174 a thread's merge does not run inside ccmuxd, and since task
#175 neither does a queued task's merge (rebase, checks, fast-forward) or the
tidy-up after it. Each is a job: one run of the systemd user unit
`ccmux-job@<id>`, which is this binary as `ccmuxd job-exec <spec>`
(`daemon/internal/jobs`). The daemon writes the spec, starts the unit, and acts
on the result file when the unit has ended. A deploy, a crash of the daemon or
a reboot during a merge neither loses it nor runs it twice. The daemon keeps
every decision: what a failed merge does (back to its worker, back in line, to
a person), the task's row, the slots.

- **The kinds:** `thread-merge` (`merge-thread-<n>-<hex>`), `task-merge`
  (`merge-task-<n>-<hex>`) and `task-tidy` (`tidy-task-<n>-<hex>`). The first
  two are the same landing (`taskq.RunLand`); the third removes a merged task's
  worktrees, branches and briefs once the daemon has closed its workers.
- **A task's row is the guard.** The end of a task's merge is acted on only
  while the row still says `merging`, and acting on it moves the row off
  `merging` first. So every path in `taskq/mergejob.go` that calls a merge job
  settled must leave another status behind, and one that could not save
  returns an error. A task that says `merging` with no open job is put back in
  line; "the jobs could not be read" never counts as "no job".
- **One task merge at a time**, tidy-up included: the line is taken while a
  `task-merge` or `task-tidy` job is open (the `jobs` table says, not the
  daemon's memory).
- **What a worker is told is a row first** (`task_notices`, the outbox in
  `taskq/notices.go`): saved with the rows that send the part back, delivered
  on every pass until the bus took it, under a fixed id the bus keeps one
  message for. Send a worker anything about a merge through it, never with a
  bare `TellWorker`.
- **Watch one:** `journalctl --user -u 'ccmux-job@<id>'` is its log (the checks'
  own lines are in it); `systemctl --user list-units 'ccmux-job@*'` lists the
  ones running. The thread page shows the step and the time, and
  `GET /v1/threads/{id}` carries it as `merge`. A task's merge has no page of
  its own yet: its id is in ccmuxd's log (`jobs: merge-task-<n>-<hex> started`)
  and in the `jobs` table under subject `task:<n>`. Its files are in
  `~/.local/state/ccmux/jobs/<id>/`: `spec.json`, `journal.jsonl` (the steps the
  job finished), `result.json`, and `applied.jsonl` (what the daemon did with
  the result). The `jobs` table is the daemon's record. Files go two weeks after
  a job ended; rows stay.
- **The daemon is the only writer of the database.** A job writes files. What
  the daemon does with a result (`jobs.Handler`) is called again after a crash,
  so every handler must be safe to repeat: record each step in the applied
  journal, and name a message to a session after the job (`SendAs`), never
  `Send`.
- **A job can be run again at any time**, so every step must be safe to repeat
  too. The journal saves the minutes (checks that passed on a commit are not run
  again on it); it is not what makes a rerun correct.
- **"Is it dead" is systemd's to say, in so many words.** Only `inactive` or
  `failed` counts (`jobs.UnitState.Dead`). No answer, or any other state, means
  keep watching: taking a hiccup for a death would run a merge twice.
- A job that ends with no result is started again, three times at most; then
  it is lost: a thread is open again and its lead is told, a task reads merge
  failed and its note says what had landed. The unit stops a
  job at 6 hours (`RuntimeMaxSec`), a merge stops itself at 3.
- One job at a time works in a repo: it holds `~/.local/state/ccmux/locks/<repo>-<hash>.lock`
  (flock, so a dead job holds nothing). Thread merges, task merges and task
  tidy-ups all take it, so they never work in one repo at once.
- The unit caps a job at 75% of memory and every core but one, and kills its
  whole process group when it stops. A check sees the daemon's own environment
  (it rides in the spec, which is why the spec is 0600).
- Spec, result and what the daemon keeps of a merge carry a format number. A
  daemon that reads another one says so and guesses nothing: the thread is open
  again and the lead is told to look at the trunks. Raise `jobs.Version` (or
  `askFormat` in `threads/mergejob.go` or `taskq/mergejob.go`) when a format
  changes.
- **A scratch daemon runs no jobs unless it is given `-job-unit-prefix <its
  own name>`**, and any daemon that finds a template for another daemon's job
  folder leaves it alone. Without jobs, a thread's merge is refused with the
  reason, and a built task waits and says so in its note. The same holds on
  macOS and any host without systemd: nothing merges in the daemon any more.
  A test that names a prefix must stop its units and delete
  `~/.config/systemd/user/<prefix>@.service` afterwards.
- A new kind of job is a `jobs.Runner` in `jobKinds` and a `jobs.Handler` in
  `wireJobs` (`cmd/ccmuxd/jobs.go`), and a case in `workOf`
  (`cmd/ccmuxd/sandbox.go`): with the sandbox on, a job whose work the walls
  do not know is not started. Keep `internal/jobs` free of the packages
  that own the work.

## Caps and walls: the sandbox (Linux)

Since task #178 every session service and every job service runs under caps,
and task and thread work can be walled into its worktree
(`daemon/internal/walls`, wired in `cmd/ccmuxd/sandbox.go`). Settings >
Sandbox holds both (`GET/PUT /v1/sandbox`, `POST /v1/sandbox/check`), and
shows what this host allows with the command that fixes what it lacks.

- **Caps are in the two unit templates** (`MemoryMax`, `CPUQuota`,
  `TasksMax`, `NoNewPrivileges=yes`, and `RuntimeMaxSec` for a job). Defaults:
  75% of memory, every core but one, 8192 processes, 6 hours. A changed cap
  rewrites the templates and reloads systemd, which **applies it to the
  running units at once** (seen live), a deploy included. So keep the
  defaults loose: they stop a runaway, nothing else.
- **Walls are a drop-in file per unit**
  (`~/.config/systemd/user/<unit>.d/50-ccmux-walls.conf`), because the paths
  differ per worktree and the template is shared. A session's is rewritten at
  every start from what the setting says then, and removed when the session
  ends or starts open; a job's is written once and removed when the job has
  ended. A test that names a unit prefix must remove what is left of these.
- **Only task and thread work is walled:** sessions with a role (a thread's
  lead and helpers, a task's builders) and the merge and tidy-up jobs. A
  repo's own session, a person's plain session and a role agent run under
  the caps only.
- **`ProtectSystem=strict` leaves `/home`, `/root` and `/run/user`
  writable** (seen live on systemd 255): `ProtectHome=read-only` is what
  closes home, and both are always written together.
- **What is open** (`walls.WorktreeWalls`, `walls.RepoWalls`, `walls.Caches`,
  `walls.ClaudeHome`): a session gets its worktree, of the repo's `.git` only
  `objects`, `refs`, `logs` and its own `worktrees/<name>` entry, and under
  home `~/.claude/projects` and caches of the walled units' own (next
  point). A merge or tidy-up job gets each repo's own folder (a fast-forward
  rewrites it) with `.git/hooks` and `.git/config` sealed read-only, the
  folder its worktrees are in, and the same under home. A tidy-up has no
  network; a merge keeps it (checks fetch). More folders: "Extra folders" in
  Settings.
- **The caches behind walls are not the person's** (`walls.ApartCaches`,
  task #183): in the place of `~/.cache`, `~/go/pkg/mod` and `~/.npm` a
  walled unit sees folders under `~/.local/state/ccmux/walled-caches`,
  which walled units share with each other and nothing outside the walls
  reads. The person's own hold what their other sessions run: the Python
  environment of an MCP server (`~/.cache/uv`, seen on this host), browsers,
  compiled Go packages. The price is one cold cache: the first walled build
  compiles and downloads everything. What a repo's checks need from a cache
  (Playwright's browsers, say) has to be installed once from behind the
  walls.
- **Never open more of `.git` to a session, and never `~/go`, `~/.config`,
  `~/.claude` or `~/.claude.json`.** Hooks, the config (hooksPath,
  fsmonitor), anything on the PATH and everything Claude Code reads (its
  settings and their hooks, the hook scripts, plugins, skills, commands,
  agents, `CLAUDE.md`, the MCP servers in `~/.claude.json`) would run later,
  outside the walls.
- **Of `~/.claude` a list is opened, the folder never** (`walls/claude.go`,
  task #183). A folder Claude Code reads that is not there yet
  (`~/.claude/agents`) cannot be sealed by name, so the top folder stays
  read-only and only `projects/` (conversations, memory) is opened. What a
  session sources before every shell command (`shell-snapshots/`,
  `session-env/`), and `sessions/` and a few more, are each walled unit's own
  (`Plan.Private`, a bind of a folder in the unit's own state over the shared
  one): shared, a walled unit would write another session's and run there.
  Seen live 2026-11-20 on Claude Code 2.1.329, with a haiku builder on a
  scratch daemon: a turn, a commit, the SessionStart gate snapshot and
  `qa-receipt.sh sign` work this way (all four write the worktree's git
  folder and `/tmp`). The qa commit review ran and wrote its log there too;
  its reviewers answered only in the earlier trial, where the session
  folders were still shared (on a scratch daemon they have no account to
  ask with). Refused and harmless: the plugins' bookkeeping and
  `~/.claude.json`. The login is not renewed from behind
  walls (the refresh lock is in `~/.claude` itself, so Claude Code sends no
  refresh token; read in its code); a walled session reads the login again
  when a session outside has renewed it, and with an account on the proxy it
  does not matter.
- **A unit sees its own part of the folders every unit shares** (`walls.Own`:
  an empty read-only folder with the unit's own files bound back in). For a
  session that is the sessions' state (other sessions' specs carry their bus
  tokens) and the sockets' folder: a walled session's socket is in
  `<runtime>/s/<id>.d/sock` (`runner.Paths.Socket` says which is current),
  because a runner takes a prompt from whoever connects. For a job it is the
  jobs' folder. Each kind's folders are hidden whole from the other kind.
- **Hidden from every walled unit** (`cmd/ccmuxd/sandbox.go` `secrets`): the
  folder with the API token (below), the daemon's config folder (registry,
  `peers-secret`, tailnet key), the clipboard token, the user's bus, and
  Claude Code's own sockets (`/run/user/<uid>/cc-socks`). Hide a folder, not
  a file the daemon writes again: a file replaced by a rename is a new file,
  which a unit that was already running then sees.
- **The self-check is the guarantee** (`walls.Check`). On a host that does
  not allow walls systemd starts the unit fully open and says nothing. So a
  walled unit's spec carries what it must find (`Spec.Walls`: another mount
  namespace than the daemon's, the paths that must refuse a write as a
  read-only file system, the folders that must take one), and `job-exec` and
  `runner-exec` check before any work. A job that fails it ends with a
  result that says so (the task reads merge failed, for a person). The check
  also tries what is hidden (a socket takes no connection, a file does not
  open, a folder does not list), that a shared folder shows only the unit's
  own, and that the nearest process outside the walls cannot be walked into
  through `/proc/<pid>/root` (closed by the user namespace systemd builds a
  user unit's walls in; seen live). A session
  exits with status 97 (`walls.ExitRefused`), which systemd does not restart
  (`RestartPreventExitStatus`); the supervisor puts it to sleep with the
  reason, and a builder's part carries that reason. Before that, the daemon
  refuses to start a walled unit at all when its probe of the host (a real
  trial unit, `ccmuxd walls-probe`) says no. Settings that cannot be read
  start nothing: "off" is never guessed.
- **A walled session keeps its state file in `<state>/<id>.own/`**, not
  beside every other session's spec, which it must not be able to rewrite
  (systemd restarts a crashed session on the spec that is on disk). The file
  follows the session when it becomes walled or open (`moveState`).
- **Claude Code's own sandbox for shell commands is a second switch, off by
  default** (`claudeserve/shellbox.mjs`, `--shell-box-write`). Seen live: a
  sandboxed command has a network of its own, so it reaches no loopback
  address (not a builder's dev servers, not this daemon) and only the hosts
  the person's Claude settings allow. Switched on where bubblewrap or socat
  is missing, walled sessions do not start, and say why.
- **The user's bus is hidden** (`InaccessiblePaths`, `sandbox.go` `userBus`),
  and the self-check tries a real connection to it: through it
  `systemd-run --user` ran a command outside the walls (seen live).
- **The two holes of the v0.1.131 review are closed (task #183): Claude
  Code's config is sealed, and the daemon's API asks a caller on this
  machine for a token** (next section). Seen live on a scratch daemon: a
  walled haiku builder was refused at every one of them, did its turn,
  committed, reported over the bus, and its merge and tidy-up landed walled.
- **What the walls still do not stop.** A walled session can message a
  session outside the walls over the bus (and start it with
  `spawn_if_missing`); it reads as a peer's message, not a person's, and the
  other Claude decides. It writes `~/.claude/projects` (its repo's memory is
  shared with the repo's own session, by Claude Code's design, and so are
  other sessions' conversations there). It keeps its network, and home is
  readable: what the person can push or log in to with the keys in home, it
  can too (`git push`, `ssh localhost`, a docker socket), and from another
  tailnet machine it reaches with them the daemon does not ask for the
  token. It reaches every loopback port: dev servers, databases, and the
  chat port of an agent on another harness (opencode and the like), which
  takes a prompt with no token and is not walled.
- **Claude Code cannot save `~/.claude.json` from behind walls** (it writes
  through a lock folder and a temp file in home; traced live). The turn
  works; what it would have saved there is dropped.
- A walled path cannot be under `/tmp` (the unit's `/tmp` is its own), and a
  folder its walls name must exist, or systemd fails the unit with status
  226 and no path; `Plan.Validate` refuses both first, by name.

## The API asks callers on this machine for a token

Since task #183 every `/v1` route wants the daemon's API token from a
caller on this machine (`api/localgate.go`). A walled session shares the
host's network, so without it `PUT /v1/sandbox` switched the walls off and
`POST /v1/sessions` started a session outside them.

- **The token is new at every daemon start**, in
  `<runtime>/private/api-token` (0600, in a 0700 folder the walls hide;
  `cmd/ccmuxd/apitoken.go`). It is never in an environment, a spec or the
  log.
- **Never print the token in a session.** Read it where it is used:
  `curl -H "Authorization: Bearer $(ccmuxd api-token)" ...`. What a session
  prints is saved in its transcript under `~/.claude/projects`, and a walled
  session reads all of home. A token that was printed is good until the
  daemon restarts.
- **"This machine" is loopback and every address of the host's own
  interfaces**, whatever listener the call came in on. This host is on the
  tailnet as the person, so a walled session calling the daemon's tailnet
  name would otherwise be vouched for by WhoIs. A caller from another
  machine is not asked: nothing changes for the lenses over the tailnet.
- **Refusing is the rule, the open routes are the list** (`localOpen`,
  `paneOpen`, `paneOwn`). Open: `/v1/health`, `/v1/whoami`, the clipboard
  pipe (its own token), the bus routes the shim calls (each checks the
  caller's pane token; the bus socket only in its peer mode, since
  `mode=listen` streams a group to whoever asks), and under
  `/v1/panes/{id}/` the tools a session calls for itself, for which the
  gate checks **that pane's own token** (they took any local caller that
  named a pane before). Three pane routes take no token at all, because
  what calls them sends none: `agent-signal` and `bus-context` (the
  opencode plugin) and `driver` (the co-author git hook). A new route is
  gated until it is put on a list. Never put a route there that starts,
  prompts or configures anything on a pane token alone, and read a handler
  for a mode that checks nothing before opening its path.
- **Only a plain path is ever open**: the mux matches escaped pieces, so
  `/v1/windows/w/agents/..%2F..%2F..%2Fhealth` is the agent route though it
  cleans to `/v1/health`.
- **A browser on this machine** opens the link `ccmuxd lens-url` prints
  (`http://ccmux.localhost:<port>/?key=...`). The key is not the token: the
  command asks the daemon for it (`POST /v1/lens-key`), and it works once,
  within two minutes, so a link a session printed is worth nothing in its
  transcript. The gate turns it into an HttpOnly cookie that holds the
  token and redirects; the lenses' requests and websockets need no
  change. The name `ccmux.localhost` is on purpose: a cookie belongs to a
  host name whatever the port, so one set for `127.0.0.1` would be sent to
  every dev server on this machine. That narrows it and does not close it: a
  page that sends the browser to `ccmux.localhost:<its own port>` still gets
  the cookie. A page opened here without the key gets a page that says so.
  After a daemon restart a new link is needed.
- `GET /v1/health` carries `localGate`: false means the API is open to every
  local process. A daemon that cannot make the token does not start.
- The LLM proxy (`/llm/pane/...`, its own port) is not gated: sessions need
  it, and it starts and configures nothing.

## A queued task's builders are sessions (Linux)

Since task #176 the Claude that builds one part of a queued task is a runner
session (`ccmux-session@<id>`, Agent SDK), not a Claude typed into a tmux
pane. A task is a thread the queue leads: a row in `threads` with the task's
number (`task_id`) and no lead session, whose members are builders (role
`builder`), one per part, each in its part's worktree from its first moment.
The queue still owns everything it did: parts, starts-after, slots, the
watchdog, reports, the merge and tidy-up jobs, send-back.

- **The seam is `taskq.Host.StartBuilder`** (`threads/builders.go` makes the
  thread and the session). Its answer is the session's pane id, which is what
  a part's `pane_id` holds and how the tasks tool, the bus and the watchdog
  know the builder. A daemon with no session runners starts none, and waiting
  tasks say so in their note.
- **The pane's stamp is what makes it a worker.** A builder's runner pane
  carries `createdBy: "task-runner"` like the tmux workers before it
  (`manager/builders.go`); its signals, its dev servers' ports and the
  clearing of its worktree all key on that. Builders in a worktree are read
  from the session rows (`store.BuildersIn`), never from the pane list.
- **A runner's signals are translated for the watchdog** (`builderSignal`):
  busy is "at work" but never replaces a prompt's mark, and stopped is a turn
  that ended, not a person typing. "A person typed" comes from the prompt
  route (`api.personTyped`), because a runner's own prompt hook is not heard.
  A new way to prompt a builder as a person must call `NoteTaskPrompt` too.
- **Gone means asleep.** A builder's Claude counts as exited when its row
  says asleep, never when its unit is down between two restarts.
- **Names are the task's**: `<repo>-task-<n>` on the bus. A repo's plain name
  is still the repo's own session; a task's thread reroutes nothing and
  stands up no helper (`store.Threads` leaves it out on purpose, so the
  routing, the thread merge and its recovery never meet one).
- **The brief is the file beside the worktree** (`taskq.BriefFile`) and is
  the session's standing prompt at every start; a missing one fails the
  start. The tidy-up removes it after the session has ended.
- **A builder runs with** its part's model and effort, `--permission-mode
  auto`, the login shell's PATH, no loadout (it loads the person's whole
  Claude setup, as it did in a pane, so the repo's gates and commit review
  come along), and `--keep-out-files <repo>`: its file tools cannot write in
  the repo's own folder, its shell is not judged (its brief tells it to `cp`
  env files out of that folder).
- **A builder's dev servers die with its unit** (`KillMode=control-group`)
  when it is restarted or ended. Ports still come from the part's block.
- **Tmux workers from before still finish the old way.** `TaskPane`,
  `ClearTaskFolder` and `TellWorker` serve both; `Manager.StartTaskPane` is
  kept only for them and their tests, and goes when they are gone.
- "Open session" on a part opens the builder's conversation
  (`/threads.html?s=<session>`); the part's view carries `sessionId`.

## A main session asks other repos through side helpers (Linux)

A repo's main session (kind `lead`, in no thread: the repo's own session on
the thread page) that names another repo of its window by its plain name
(`send_message` or `delegate`) reaches a helper of its own for that repo,
`<repo>-for-<its repo>`, never that repo's old terminal session and never
that repo's own main session (`internal/threads/side.go`).

- **The helpers are a side thread** (`threads.kind = 'side'`, led by the
  main session, which itself keeps `thread_id` 0). Its row is made with its
  first helper; the member index adds every main session's pane
  (`indexSides`), so `Route` works before the row exists (`Lead` set,
  `ThreadID` 0).
- **A helper reads, checks and answers, and never lands a change.** Its
  first non-read command moves it into a scratch worktree (the thread move,
  with its own notice), where it runs anything; the side thread is never
  merged, and its scratch copy and branch are force-removed when it ends
  (`taskq.DropScratch`). Real work in another repo is a task part.
- **It ends when its cache is cold** (`threads.ExpireSide`, every minute,
  over `runner.Supervisor.Cold`): idle past the last turn's cache lifetime
  (1 hour on this host), no card, no message waiting, a finished turn, and
  no delegation it still owes. The next question starts a fresh one. The
  same pass closes a side thread whose main session ended without its
  helpers being stood down (`OpenSideThreads`, which does not filter on the
  lead the way `Threads()` does). A
  message that reaches a session just after it ended is handed back to its
  sender (`peers bounce`).
- **It also ends when its main session moves on** (`Service.MainMovedOn`):
  a new conversation that was really made (a clean start or Start over,
  heard from the runner's `CleanStarted`; a refused one or a resend is not
  heard) and filing a task each stand the helpers down. `CleanStarted` runs
  on its own goroutine, off the session's lock: a person's stand-down holds
  the side thread's lock while it tells the main session, so calling it
  under the session's lock deadlocked (caught in review, test
  `TestCleanStartedRunsOffTheSessionsLock`).
- A helper naming its main session's repo reaches the main session itself
  (`HelperRoute.Pane`). A main session ending stands its helpers down.
- **Not covered:** role agents, task builders and old terminal sessions that
  name a repo keep the old routing (they can still start a terminal Claude).
  The tasks tool still wants a part in the filing repo.
- Main sessions read their own rule on the bus (`buscontext.Reader.Main`).

## Start over, not Claude Code's /clear (Linux)

A Threads session's "Start over" (`POST /v1/sessions/{id}/start-over`,
`runner.Supervisor.StartOver`) is a clean start with no message: a new
conversation in the same row, the old one kept above a divider. The thread
page turns a typed `/clear` into it, and never sends `/clear` to Claude
Code in a runner session: it switches conversation inside the runner, the
daemon never hears, and the next restart resumes the old one (seen live
2026-11-21: the cleared word came back after a restart). An agent has no
Start over (its base says when it starts fresh: Sleep marks the next start
fresh); its typed `/clear` is refused with that.

## Claude role agents are sessions (Linux)

Since task #177 a role agent on the claude harness (radar, x-post and the
rest) is a runner session (`ccmux-session@<id>`), not the sidecar typed into
a tmux pane on a loopback port. Its home is still its workspace in the
window (the row whose `agent` names its base), with no tmux session and no
panes behind it. The home has one session that has not ended, of kind
`agent`, kept across sleeps: so its pane id, its peer on the bus and its
mailbox stay the same. tmux is no longer what keeps an agent alive.

- **Where an instance runs is decided by where it is** (`api.onSession`):
  this daemon runs sessions, the base is on the claude harness, and the
  instance is not running in a tmux pane right now. A claude agent that was
  running in its pane at the deploy keeps every pane path (chat port, push,
  ctrl-d) until that run ends, and moves at its next start
  (`api.makeAgentSession`): its session is made under the old pane's id
  first, and only then is its tmux session closed and its pane rows removed
  (`manager.TmuxAgentPane`, then `RetireAgentTmux`), so a session that could
  not be made costs the agent nothing. What the bus had already sent the
  pane is marked read before that (`peers.SettlePaneMailbox`), or it would
  all run again as prompts. Other harnesses
  (opencode, pi, codex) and hosts without systemd keep the pane, unchanged.
- **Its spec is its base's, read at every start** (`runner.Config.Agent` →
  `api.AgentStart`): the same flags the pane's launch line carries
  (`agent.ClaudeRunnerArgs`; a test holds the two together), its `.env`
  files' values in the spec's environment (the spec is 0600 for that), the
  login shell's PATH, no loadout and no brief. Its flags come after the
  runner's own, where the sidecar keeps the last `--setting-sources`: that is
  how an isolated base wins. It runs the SDK's own Claude Code, as it did in
  its pane (`--claude` is not passed).
- **Sleep has one door** (`api.SleepAgentSession` → `runner.Supervisor.Sleep`):
  the idle exit, a person's Sleep, the editor's restart and the thread
  page's `/wanted` all go through it. It stops the unit under the session's
  lock, refuses while a prompt has not been read, and marks the next start
  fresh unless the base says `start: continue`. Put an agent to sleep any
  other way and it wakes in the wrong conversation.
- **Fresh means a new conversation in the same row.** `sessions.conversation`
  is the Claude conversation the row runs (empty: its own id), and
  `fresh_next` makes the next start open a new one. A crash, "Restart now" or
  a reboot never set the mark: they resume. Read a session's conversation
  with `RunnerSession.ConversationID()`, never its id.
- **A message wakes it like any session** (the bus hands it to the inbox),
  so `PushToAgentPane` and `WakePane` are never used for it. The cap on
  running agents still holds: a message for an asleep agent over the cap
  stays in its mailbox and is handed over at the bus's next sweep
  (`api.agentWakeHeld`); a wake from the thread page over the cap is a 409.
- **The lifecycle loop reads its pane** (`manager/agentsessions.go`): the
  runner pane carries `agent` and `agentVersion`, so idle exit, keep-alive,
  drift and the cap work as for a pane. Its idle clock is not in `panes`:
  after a restart it comes back from the attention on its session row.
- **Lenses:** the thread page opens an agent's conversation
  (`/threads.html?s=<session>`), and the old lens sends a home with no panes
  there. The old chat routes (`/v1/panes/{id}/agent...`) answer 409 for a
  session's pane, and the attention board has no tile for one.
- **A scratch daemon's agents are sessions too**, under its own
  `-runner-unit-prefix`, and load the `-peers-shim` it names. Never start
  the real role agents in a test: make a base on Haiku.

## Memory is tidied every night

Every session runs on Claude Code's own prompt (the `claude_code` preset,
`claudeserve/agent.mjs`), with a brief appended when there is one. Before
2026-11-24 a session with no brief (every repo's main session) ran on the
SDK's bare prompt and was never told how to save memory.

- **A worktree shares its repo's memory** (Claude Code files it under the
  repo; tested 2026-11-24), so builders and helpers read and write the
  repo's own `MEMORY.md`. The builder brief asks for lessons to be saved.
- **The nightly tidy-up** (`internal/memtidy`, `cmd/ccmuxd/memtidy.go`,
  from `-memory-tidy-hour`, 3 by default, on `-memory-tidy-model`, sonnet):
  per repo with new conversations since its bookmark (the session rows
  give the folders, worktrees included), a headless `claude -p` with file
  tools only and none of the person's settings edits a copy of the memory
  in `~/.local/state/ccmux/memory-tidy/work/`. The daemon then refuses a
  copy with no index, an index past Claude Code's load limit (200 lines,
  25,000 bytes, read in its code), half the notes removed, or a new
  secret; skips it if a session wrote the memory meanwhile; and otherwise
  backs the old one up (`backups/`, seven kept) and puts the new one in
  place file by file. Each night is a line in `log.jsonl` there and in
  ccmuxd's log (`memory tidy:`).
- **Its Claude reads and writes only its work folder because no tool is
  on its allow list** (`memtidy/headless.go`). Listed there, a tool is
  taken on any path: a Write landed in home (tested 2026-11-24), and a
  Read could copy a hidden secret into memory walled sessions read.
  Unlisted, `acceptEdits` takes reads and edits inside the folder, and
  anything outside asks, which `-p` refuses (tested: Read, Grep, Glob of
  home and a sibling folder). It has no walls; never list a tool there.
  It cannot delete, so a note it empties is removed.
- **It runs in the daemon, not as a job**: a deploy cuts it, nothing is
  applied, and the next quarter hour runs it again. On a host that never
  ran one, the first pass waits for the next night.
- **It never writes through a link** (`memtidy/liveroot.go`): a walled
  session can write `~/.claude/projects`, so it could plant one. The live
  memory is written through an `os.Root` (a link out of it is refused),
  the memory folder and its project folder must be real folders, and
  temp files are new ones of their own. A night with a note under a
  linked folder fails before anything is written. A write that fails
  partway is logged as `partial` with what landed and the backup folder
  to put back from.
- A busy night reads the oldest conversations up to its budget and
  leaves the rest for the next (`deferred` in the log).
- **Claude Code's own auto-dream** does the same and stays off while
  Anthropic's switch for it is off on this account (`tengu_onyx_plover`
  in `~/.claude.json`); when it comes on, ours stands down
  (`memtidy.DreamLive`), so the two never tidy one memory.
- A scratch daemon tidies nothing unless given `-memory-tidy-hour`.
  `MEMTIDY_LIVE=<repo> go test -run TestLiveTidyOnACopy ./internal/memtidy`
  runs a real one on a copy (about 2 minutes on sonnet).
- Role agents are left out: their memory is in their own home, in two
  folders that have drifted apart (on the to-do list).

## Sessions stay current by themselves (Linux)

- **A person's session restarts itself once it has run 20 hours**
  (`-session-refresh-after`, `runner/refresh.go`), on the same conversation,
  the first time nothing would be lost: no turn, no card, no message
  waiting, no background task, and its prompt cache already cold. That is
  how a lead picks up Claude Code (specs name the `~/.local/bin/claude`
  link, which Claude Code's updater moves), plugins, skills and the SDK.
  It goes sooner, on the same quiet, when its skills or plugins changed
  (the page's "Skills changed" bar, `Config.StaleLoadouts`) or it started
  on another Claude Code, SDK or runner code than a start now would run
  (its spec's `runs` mark against `Config.RunsNow`; the runner's code is
  `agent.ClaudeServeCodeHash`, since a deploy rewrites its files in
  place), once per change. A spec from
  before the mark existed waits for the 20 hours.
  One session at a time; one that does not come back stops the rest and
  says so in its note. Role agents and task builders are left out.
- **The SDK moves daily without a release** (`internal/sdkupdate`,
  `agent/sdkchannel.go`): the newest on npm goes into
  `<agents>/.ccmux/claude-sdk/<version>/`, a real Haiku session runs on it
  (host claude and the SDK's own), and only then does `current.json` send
  new starts there. A failure is kept in `status.json` and shown in
  Settings > Agents. The pinned install in `.ccmux/claude-serve` stays and
  serves when nothing newer passed; a newer pin wins over `current`.
- A scratch daemon tries no update unless given `-sdk-update-every`.

## Shipping

`/ship` runs direct-trunk here: `main` is both the working branch and the
release trunk, there is no `dev`. Pushing `main` fires the Coolify webhook and
deploys, so there is no safe intermediate state — the advisory review runs
BEFORE the push, and against the previous `v*` tag, because `origin/main...HEAD`
is empty on this branch model and would review nothing.

**Deploying is local (2026-11-02, user's call).** Nothing builds on GitHub: the
release workflow runs by hand only, and the last published release is v0.1.88.
After `/ship` pushes the tag it runs `./build-daemon.sh` from the clean tagged
checkout on verkstan (listed in `.claude/ship-after-release`). It builds ccmuxd and ccmux-peers, installs them in
`~/.local/bin` (by rename, never by writing into a running binary), restarts
the service and fails unless the running daemon reports the version it just
built, and unless the LLM proxy service answers with this build's proxy code;
that is the deploy, and its health line is the live check. The web lens is embedded in ccmuxd, so it ships with it. Never run
`ccmuxd upgrade` without a tag here: it fetches the latest GitHub release and
would put verkstan back on v0.1.88.

Since 2026-11-04 verkstan is the only daemon: the two Mac daemons
(`patric-ccmux`, `dasha-ccmux`) were uninstalled and their tailnet nodes
deleted. Check `GET /v1/hosts` before relying on that (from this machine
with the API token, as every `/v1` call but `/v1/health`: `curl -H
"Authorization: Bearer $(ccmuxd api-token)" ...`). A host added later
that is not verkstan is not updated by `/ship`; it needs a GitHub release run
by hand and `POST /v1/hosts/{id}/upgrade`, or `./build-daemon.sh` on it.
