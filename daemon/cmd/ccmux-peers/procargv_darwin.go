package main

import (
	"bytes"
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// readProcessArgv returns a process's argv via sysctl KERN_PROCARGS2, the only
// way to read another process's arguments on macOS. The false return means
// "could not read", which the caller treats as "do not conclude anything" —
// never as "no flag".
//
// KERN_PROCARGS2 lays the buffer out as: int32 argc, the executable path, NUL
// padding, then argc NUL-terminated arguments (the environment follows, and is
// deliberately not read here).
func readProcessArgv(pid int) ([]string, bool) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(buf) < 4 {
		return nil, false
	}
	argc := int(binary.LittleEndian.Uint32(buf[:4]))
	if argc <= 0 {
		return nil, false
	}
	rest := buf[4:]

	// Skip the exec path and the NUL padding that follows it.
	if i := bytes.IndexByte(rest, 0); i >= 0 {
		rest = rest[i:]
	} else {
		return nil, false
	}
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}

	argv := make([]string, 0, argc)
	for len(argv) < argc {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			argv = append(argv, string(rest))
			break
		}
		argv = append(argv, string(rest[:i]))
		rest = rest[i+1:]
	}
	if len(argv) == 0 {
		return nil, false
	}
	return argv, true
}
