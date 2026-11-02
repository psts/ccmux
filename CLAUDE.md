# ccmux

## The two lenses never drift

**PAUSED since 2026-11-02 (user's call).** The web lens is the focus while it is
brought up to par: new work goes in `daemon/web/` only, and the Mac app stays as
it is. Its build is off too (see Shipping). When the
Mac build is turned back on, the Mac app is caught up to the web lens first, and
the rule below applies again. Until then, do not add Swift changes for parity,
and do not report a missing Mac side as a gap.

The Mac app (`Sources/ccmux/`) and the web lens (`daemon/web/`) must expose the
same functionality. The Mac app is the primary surface (used 99% of the time),
but every feature, rule, and state change ships in BOTH lenses in the same
commit — menu entries, buttons, sheet fields, and the conditions that show or
hide them. If a behavior rule lives in one lens ("running means the pane exists
AND the port answers"), the other lens implements the identical rule, not an
approximation.

When you touch one lens, grep the other for the same feature and update it.
Note: the Swift side cannot be built or tested on this Linux host, so say so
when reporting — but that never excuses skipping the change. Only the release
workflow builds it on macOS, and since 2026-11-02 that workflow runs by hand
only, and the Mac job only with the repo variable `BUILD_MAC_APP=true`: until
someone does that, nothing compiles the Swift side at all. No CI runs `swift test`: the `qa-gates` line for it
fires only where a Swift toolchain exists, so `Tests/ccmuxTests/` executes on a
developer's Mac and nowhere else. Known hole, needs a Mac to close.

## What a signed receipt covers

`.claude/qa-gates` is the list; read it there rather than trusting a copy here.
What it does not say: the two Swift lines skip on this host for want of a
toolchain, so a green signed here means the Go daemon and the web lens's
syntax, not the Mac app. Gates that need a tool this host lacks FAIL rather
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
After `/ship` pushes the tag, run `./build-daemon.sh` from the clean tagged
checkout on verkstan. It builds ccmuxd and ccmux-peers, installs them in
`~/.local/bin`, restarts the service and fails unless the running daemon reports
the version it just built; that is the deploy, and its health line is the live
check. The web lens is embedded in ccmuxd, so it ships with it. Never run
`ccmuxd upgrade` without a tag here: it fetches the latest GitHub release and
would put verkstan back on v0.1.88.
