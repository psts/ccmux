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
func readProcessArgv(pid int) ([]string, bool) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		logf("cannot read argv of pid %d: kern.procargs2: %v", pid, err)
		return nil, false
	}
	return parseProcargs2(buf)
}

// parseProcargs2 decodes a KERN_PROCARGS2 buffer: int32 argc, the executable
// path, NUL padding, then argc NUL-terminated arguments — with the environment
// following, which is why the walk is bounded by argc rather than by the buffer.
//
// Split from the syscall so it can be table-tested anywhere. It decides whether
// a session is silenced, and it only ever runs on the platform it cannot be
// tested on otherwise.
func parseProcargs2(buf []byte) ([]string, bool) {
	if len(buf) < 4 {
		return nil, false
	}
	argc := int(binary.LittleEndian.Uint32(buf[:4]))
	if argc <= 0 {
		return nil, false
	}
	rest := buf[4:]

	// Skip the exec path, then the NUL padding between it and argv[0].
	//
	// Padding and an empty argv[0] are the same bytes, so this cannot tell them
	// apart: a process exec'd with an empty argv[0] parses one argument short and
	// picks up the first environment entry to fill the count. Nothing launches
	// claude that way, and the argc bound below stops the walk regardless, so the
	// ambiguity is recorded rather than papered over.
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return nil, false
	}
	rest = rest[end:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}

	argv := make([]string, 0, argc)
	for len(argv) < argc && len(rest) > 0 {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			argv = append(argv, string(rest))
			break
		}
		argv = append(argv, string(rest[:i]))
		rest = rest[i+1:]
	}
	if len(argv) < argc {
		// A short buffer, or a process that rewrote its argv mid-read. Reporting
		// a partial argv as complete would let a missing tail read as a missing
		// flag, which silences the session.
		return nil, false
	}
	return argv, true
}
