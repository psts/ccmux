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
Note: the Swift side cannot be built or tested on this Linux host (the release
tag job builds it), so say so when reporting — but that never excuses skipping
the change.
