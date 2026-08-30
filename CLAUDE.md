# ccmux

## The two lenses never drift

The Mac app (`Sources/ccmux/`) and the web lens (`daemon/web/`) must expose the
same functionality. The Mac app is the primary surface (used 99% of the time),
but every feature, rule, and state change ships in BOTH lenses in the same
commit — menu entries, buttons, sheet fields, and the conditions that show or
hide them. If a behavior rule lives in one lens ("running means the pane exists
AND the port answers"), the other lens implements the identical rule, not an
approximation.

When you touch one lens, grep the other for the same feature and update it.
Note: the Swift side cannot be built or tested on this Linux host, so say so
when reporting — but that never excuses skipping the change. The release tag
job BUILDS it on macOS. No CI runs `swift test`: the `qa-gates` line for it
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
when it landed (`.claude/gates/gocyclo-frozen.txt`, 28 non-test functions).
Anything new over the cap fails, and so does a frozen function that gets worse.
**The frozen list is delete-only.** Split the function, or land it under the
cap; adding a line to keep a gate quiet is how the list stops meaning anything.
Prune an entry once it drops under 10.

## Shipping

`/ship` runs direct-trunk here: `main` is both the working branch and the
release trunk, there is no `dev`. Pushing `main` fires the Coolify webhook and
deploys, so there is no safe intermediate state — the advisory review runs
BEFORE the push, and against the previous `v*` tag, because `origin/main...HEAD`
is empty on this branch model and would review nothing.
