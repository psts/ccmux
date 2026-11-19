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
