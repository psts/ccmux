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
  and every package of this module it imports (today: `llmproxy`, `harness`).
  Import `store`, `manager` or `api` there and nearly every deploy restarts the
  proxy again. It reads the daemon's database itself, read only
  (`reader.go`); the daemon stays the only writer.
- **A change under `internal/llmproxy`, `internal/harness` or
  `internal/llmserve` restarts the proxy on the next deploy**, after a 10s
  drain, and cuts whatever is still streaming then. Claude Code retries a cut
  reply by itself (seen live 2026-11-19), so it costs tokens, not work.
- A pane keeps the address it started with. Panes from before the move still
  call `http://127.0.0.1:7900/llm/pane/<id>`; the daemon passes those on to the
  service, and they get the benefit at their next start.
- Its log is its own: `journalctl --user -u ccmux-llm-proxy` (the `llm: pane …`
  lines used to be in ccmuxd's). `GET /v1/health` on the daemon carries
  `llmProxy` (mode, url, serving, what the last daemon start did to it).
- **A scratch daemon never touches the live unit.** With `CCMUXD_ROOT` set it
  keeps the proxy in its own process unless it is given `-llm-proxy-unit
  <its own name>`; and any daemon that finds a unit file serving another
  database leaves it alone. A test that names a unit must disable and delete
  that unit file afterwards.
- macOS, and any host without systemd, keep the proxy inside the daemon
  (`-llm-proxy-unit none` forces that on Linux too).
- Not moved: the meridian sidecars still run under the daemon and restart with
  it.

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
  `wireJobs` (`cmd/ccmuxd/jobs.go`). Keep `internal/jobs` free of the packages
  that own the work.

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
deleted. Check `GET /v1/hosts` before relying on that. A host added later
that is not verkstan is not updated by `/ship`; it needs a GitHub release run
by hand and `POST /v1/hosts/{id}/upgrade`, or `./build-daemon.sh` on it.
