package main

import (
	"bytes"
	"fmt"
	"os"
)

// readProcessArgv returns a process's argv from procfs. The false return means
// "could not read", which the caller treats as "do not conclude anything" —
// never as "no flag".
func readProcessArgv(pid int) ([]string, bool) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		// A hidepid mount, a pid that exited between Getppid and here, a sandbox
		// policy. Silence here would make the detection dead on every host with
		// nothing to say why.
		logf("cannot read argv of pid %d: %v", pid, err)
		return nil, false
	}
	argv := parseProcCmdline(raw)
	if len(argv) == 0 {
		return nil, false
	}
	return argv, true
}

// parseProcCmdline splits a NUL-separated /proc cmdline. Empty (a kernel thread
// or a zombie) and all-NUL both yield nil, which the caller reads as "could not
// read" rather than as an argv with no flag in it.
func parseProcCmdline(raw []byte) []string {
	trimmed := bytes.TrimRight(raw, "\x00")
	if len(trimmed) == 0 {
		return nil
	}
	parts := bytes.Split(trimmed, []byte{0})
	argv := make([]string, 0, len(parts))
	for _, p := range parts {
		argv = append(argv, string(p))
	}
	return argv
}
