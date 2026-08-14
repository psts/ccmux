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
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	// NUL-separated, usually with a trailing NUL.
	parts := bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0})
	argv := make([]string, 0, len(parts))
	for _, p := range parts {
		argv = append(argv, string(p))
	}
	return argv, true
}
