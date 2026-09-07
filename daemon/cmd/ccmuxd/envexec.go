package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"ccmux.dev/ccmuxd/internal/agent"
)

// cmdEnvExec: `ccmuxd env-exec -f FILE [-f FILE...] -- [KEY=VALUE...] PROGRAM
// [ARGS...]` loads each env file as DATA (agent.ParseEnvFile: KEY=VALUE
// lines, nothing executed), sets the variables plus the leading assignments,
// and execs the program in place. This is how an agent's .env files reach
// its harness: the launch line names the files, never the values, and a
// file an agent wrote can set variables and nothing else. Lines that are
// not KEY=VALUE are reported on stderr (the pane) and skipped.
func cmdEnvExec(args []string) error {
	fs := flag.NewFlagSet("env-exec", flag.ContinueOnError)
	var files stringList
	fs.Var(&files, "f", "env file to load (repeatable; later files win)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	vars, problems, err := agent.LoadEnvFiles(files)
	if err != nil {
		return err
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "ccmuxd env-exec:", p)
	}
	for k, v := range vars {
		os.Setenv(k, v)
	}
	for len(rest) > 0 && strings.Contains(rest[0], "=") {
		k, v, _ := strings.Cut(rest[0], "=")
		os.Setenv(k, v)
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return errors.New("env-exec: no program to run after the assignments")
	}
	path, err := exec.LookPath(rest[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, rest, os.Environ())
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }
