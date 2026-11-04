# ccmux

## One lens: the web lens

The native Mac app was removed from `main` on 2026-11-04 (user's call). Its
code (`Sources/`, `Tests/`, `SwiftTerm/`, `build-app.sh`, the release
workflow's Mac job) is kept, frozen, on the `mac-app` branch. The web lens
(`daemon/web/`) is the only lens: there is no parity rule and no Swift side to
update. Comments in the daemon and web code that mention the Mac app or
SwiftTerm describe history; they are not a to-do.

The daemon still runs on macOS (the `dasha-ccmux` host, darwin builds in
`.goreleaser.yaml`, launchd in `ccmuxd install`). Only the app is gone.

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
`~/.local/bin`, restarts the service and fails unless the running daemon reports
the version it just built; that is the deploy, and its health line is the live
check. The web lens is embedded in ccmuxd, so it ships with it. Never run
`ccmuxd upgrade` without a tag here: it fetches the latest GitHub release and
would put verkstan back on v0.1.88.

verkstan is not the only daemon: the hub also federates `patric-ccmux` and
`dasha-ccmux`, tsnet nodes on the Macs (`GET /v1/hosts`). They stay on v0.1.86
by the user's choice (2026-11-02), so a session hosted there misses daemon-side
changes such as agent-first tab order. Updating them needs a GitHub release run
by hand and `POST /v1/hosts/{id}/upgrade`.
