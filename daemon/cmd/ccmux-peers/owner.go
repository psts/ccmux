// Bus-inbox ownership: exactly one process per pane may be the pane's peer — the
// interactive Claude session — so a message to the pane is delivered to it and
// not swallowed by a sub-agent or warm spare that happens to share the pane's
// derived identity. Claude Code spawns sub-agents and spares detached (no
// controlling terminal) or in a separate process group; the interactive session
// owns the pane's pty and is its foreground group. That terminal state is a
// server-side POSIX fact, independent of whether any lens (Mac app, web) is
// attached, so it holds for headless sessions living in tmux.
package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// ownsBus is the pure rule (unit-tested): own the pane's inbox iff we hold a
// controlling terminal and that terminal's foreground process group is ours or
// our parent's. The parent case covers an MCP server spawned into its own group
// whose parent is still the foreground session; a sub-agent's MCP server is in
// neither, and a detached one has no controlling terminal at all.
func ownsBus(hasControllingTTY bool, fgPgrp, myPgrp, parentPgrp int) bool {
	if !hasControllingTTY || fgPgrp <= 0 {
		return false
	}
	return fgPgrp == myPgrp || fgPgrp == parentPgrp
}

// isBusOwner probes the live terminal state and applies ownsBus. Opening
// /dev/tty fails with ENXIO when there is no controlling terminal, which is the
// detached sub-agent/spare (and headless non-interactive) case.
func isBusOwner() bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer tty.Close()
	fg, err := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return false
	}
	myPgrp, _ := unix.Getpgid(0)
	parentPgrp, _ := unix.Getpgid(os.Getppid())
	return ownsBus(true, fg, myPgrp, parentPgrp)
}
