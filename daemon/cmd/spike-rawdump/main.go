// Command spike-rawdump attaches to a tmux session in control mode and prints
// every raw line tmux emits, with non-printable bytes made visible. It exists
// to learn tmux 3.6b's actual control-mode wire format (especially %output
// octal-escaping) before we commit parser code. Throwaway-ish: the lessons feed
// internal/tmux/control.go.
//
// Usage:
//
//	go run ./cmd/spike-rawdump -L ccmuxspike -t s1
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func main() {
	socket := flag.String("L", "ccmuxspike", "tmux socket name (-L)")
	target := flag.String("t", "s1", "session to attach")
	flag.Parse()

	// Control mode speaks over stdin/stdout pipes — no PTY needed (this is how
	// iTerm2 drives tmux over ssh).
	cmd := exec.Command("tmux", "-L", *socket, "-C", "attach", "-t", *target)
	stdin, err := cmd.StdinPipe()
	must(err)
	stdout, err := cmd.StdoutPipe()
	must(err)
	stderr, err := cmd.StderrPipe()
	must(err)
	must(cmd.Start())

	go dump("ERR", stderr)
	done := make(chan struct{})
	go func() { dump("OUT", stdout); close(done) }()

	// Drive a small sequence of commands with pauses so we can correlate the
	// %begin/%end reply blocks and %output notifications.
	send := func(s string) {
		fmt.Printf(">>> %s\n", s)
		io.WriteString(stdin, s+"\n")
	}
	time.Sleep(300 * time.Millisecond)
	send("display-message -p '#{client_flags}'")
	time.Sleep(200 * time.Millisecond)
	send("list-windows -F '#{window_id} #{window_name} #{window_active}'")
	time.Sleep(200 * time.Millisecond)
	// Produce known bytes in the pane: tab, ESC/CSI color, box-drawing, newline.
	send(`send-keys -t ` + *target + ` "printf 'A\tB\\033[31mR\\033[0m\\342\\224\\202\\n'" Enter`)
	time.Sleep(400 * time.Millisecond)
	send("capture-pane -p -t " + *target)
	time.Sleep(300 * time.Millisecond)
	send("resize-window -x 100 -y 30 -t " + *target)
	time.Sleep(300 * time.Millisecond)

	stdin.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	cmd.Wait()
}

// dump prints each line with non-printable bytes shown as \xNN so we can see the
// exact escaping tmux uses inside %output.
func dump(tag string, r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			fmt.Printf("[%s] %s\n", tag, visible(strings.TrimRight(line, "\n")))
		}
		if err != nil {
			return
		}
	}
}

func visible(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		} else {
			b.WriteString("\\x" + strconv.FormatInt(int64(c), 16))
		}
	}
	return b.String()
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
