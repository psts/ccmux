package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// procargs2 builds a KERN_PROCARGS2 buffer: argc, the exec path, NUL padding,
// the arguments, then the environment that really does follow argv in the
// kernel's buffer — which is what the argc bound exists to stay out of.
func procargs2(argc int, execPath string, padding int, args, env []string) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint32(argc))
	b.WriteString(execPath)
	for i := 0; i < padding+1; i++ {
		b.WriteByte(0)
	}
	for _, a := range args {
		b.WriteString(a)
		b.WriteByte(0)
	}
	for _, e := range env {
		b.WriteString(e)
		b.WriteByte(0)
	}
	return b.Bytes()
}

// This decoder decides whether a session is silenced, and it runs on the release
// platform while the suite usually runs on the other one. Table-testing the
// buffer walk is the only way it is exercised anywhere.
func TestParseProcargs2(t *testing.T) {
	env := []string{"PATH=/usr/bin", "HOME=/Users/x"}

	t.Run("a normal claude command line", func(t *testing.T) {
		buf := procargs2(3, "/usr/local/bin/claude", 4,
			[]string{"claude", "--channels", "server:claude-peers"}, env)
		argv, ok := parseProcargs2(buf)
		if !ok {
			t.Fatal("a well-formed buffer was rejected")
		}
		if len(argv) != 3 || argv[2] != "server:claude-peers" {
			t.Fatalf("argv = %q", argv)
		}
	})

	t.Run("stops at argc and never reads the environment", func(t *testing.T) {
		buf := procargs2(1, "/bin/claude", 0, []string{"claude"}, env)
		argv, ok := parseProcargs2(buf)
		if !ok {
			t.Fatal("rejected a valid single-argument buffer")
		}
		if len(argv) != 1 {
			t.Fatalf("argv = %q — the walk ran past argc into the environment", argv)
		}
	})

	t.Run("a short buffer is unreadable, not an argv without the flag", func(t *testing.T) {
		// argc claims four; only two are present.
		buf := procargs2(4, "/bin/claude", 0, []string{"claude", "--channels"}, nil)
		if argv, ok := parseProcargs2(buf); ok {
			t.Fatalf("a truncated buffer was reported as complete: %q", argv)
		}
	})

	t.Run("no NUL after the exec path", func(t *testing.T) {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.LittleEndian, uint32(1))
		b.WriteString("/bin/claude-with-no-terminator")
		if _, ok := parseProcargs2(b.Bytes()); ok {
			t.Fatal("a buffer with no argv at all was accepted")
		}
	})

	t.Run("extra padding between the exec path and argv", func(t *testing.T) {
		buf := procargs2(2, "/bin/claude", 16, []string{"claude", "--resume"}, env)
		argv, ok := parseProcargs2(buf)
		if !ok || len(argv) != 2 || argv[1] != "--resume" {
			t.Fatalf("argv = %q ok = %v — padding was not skipped cleanly", argv, ok)
		}
	})

	t.Run("rubbish is rejected", func(t *testing.T) {
		for _, buf := range [][]byte{nil, {1, 2}, procargs2(0, "/bin/claude", 0, nil, env)} {
			if _, ok := parseProcargs2(buf); ok {
				t.Errorf("accepted a buffer it should not have: %v", buf)
			}
		}
	})
}
