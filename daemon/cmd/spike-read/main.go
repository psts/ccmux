// Command spike-read passively attaches a tmux control-mode client and dumps
// raw lines without sending ANY commands, so the only %output is the target
// pane's own bytes. Used to confirm exact octal-escaping rules.
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
	socket := flag.String("L", "ccmuxspike", "tmux socket name")
	target := flag.String("t", "probe", "session to attach")
	ms := flag.Int("ms", 800, "how long to read")
	flag.Parse()

	cmd := exec.Command("tmux", "-L", *socket, "-C", "attach", "-t", *target)
	stdout, _ := cmd.StdoutPipe()
	stdin, _ := cmd.StdinPipe()
	_ = cmd.Start()
	go func() {
		br := bufio.NewReader(stdout)
		for {
			line, err := br.ReadString('\n')
			if len(line) > 0 {
				fmt.Println(visible(strings.TrimRight(line, "\n")))
			}
			if err != nil {
				return
			}
		}
	}()
	time.Sleep(time.Duration(*ms) * time.Millisecond)
	stdin.Close()
	cmd.Process.Kill()
	_ = io.EOF
}

func visible(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		} else {
			b.WriteString("<" + strconv.FormatInt(int64(c), 16) + ">")
		}
	}
	return b.String()
}
