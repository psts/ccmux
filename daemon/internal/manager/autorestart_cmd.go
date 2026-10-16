package manager

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"ccmux.dev/ccmuxd/internal/harness"
)

// The command an auto restart types (see autorestart_clock.go): the pane's
// startup command, plus what the running Claude carries that the startup
// command does not, plus --resume. It must start the same Claude the human
// is running, so anything it cannot account for leaves the pane alone.

// relaunchable says whether ccmux may relaunch a startup command with
// --resume appended: it must start Claude, and carry no session choice, no
// prompt and no print mode of its own that --resume would fight with.
func relaunchable(cmd string) bool {
	if !startsClaude(cmd) {
		return false
	}
	for _, f := range strings.Fields(cmd) {
		switch {
		case f == "--", f == "-c", f == "--continue", f == "-r", f == "-p", f == "--print":
			return false
		case strings.HasPrefix(f, "--resume"):
			return false
		}
	}
	return true
}

// carriedFlags are what Claude's own /restart writes into the command line it
// re-execs with; their values carry over into ccmux's relaunch.
var carriedFlags = map[string]bool{"--model": true, "--effort": true, "--permission-mode": true}

// channelsFlag is what Claude's own /restart drops, and what ccmux's relaunch
// exists to put back: a startup command may hold it while the running Claude
// does not.
const channelsFlag = "--dangerously-load-development-channels"

// relaunchFlags matches the running Claude's argv against the pane's startup
// command. It returns the carried flags to append, or false when the running
// Claude has a flag the startup command lacks, a carried flag with another
// value, or lacks a startup flag other than the channels pair: relaunching
// would then quietly change the model, the permissions or the directories the
// human chose. --resume is skipped; the relaunch adds its own.
func relaunchFlags(startup string, argv []string) ([]string, bool) {
	want := startupArgs(startup)
	have := map[string]bool{}
	var extra []string
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "--resume" || carriedFlags[a] {
			if i+1 >= len(argv) {
				return nil, false
			}
			i++
			if a != "--resume" && !flagValue(want, a, argv[i], &extra) {
				return nil, false
			}
			have[a], have[argv[i]] = true, true
			continue
		}
		if !contains(want, a) {
			return nil, false
		}
		have[a] = true
	}
	return extra, coversStartup(want, have)
}

// flagValue checks a carried flag against the startup command: absent there
// it is appended, present it must hold the same value.
func flagValue(want []string, flag, value string, extra *[]string) bool {
	quoted, ok := shellWord(value)
	if !ok {
		return false
	}
	for i, w := range want {
		if w == flag {
			return i+1 < len(want) && want[i+1] == value
		}
	}
	*extra = append(*extra, flag, quoted)
	return true
}

// shellWord makes a carried value safe to type into the pane's shell: plain
// words as they are, anything else single-quoted ("claude-opus-5-5[1m]"
// would glob). A value holding a quote or a control character is refused.
func shellWord(v string) (string, bool) {
	if safeArgRe.MatchString(v) {
		return v, true
	}
	if v == "" || strings.Contains(v, "'") || strings.ContainsFunc(v, unicode.IsControl) {
		return "", false
	}
	return "'" + v + "'", true
}

// coversStartup reports whether every startup argument is in the running
// Claude's argv, the channels pair excepted.
func coversStartup(want []string, have map[string]bool) bool {
	for i, w := range want {
		channels := w == channelsFlag || i > 0 && want[i-1] == channelsFlag
		if !have[w] && !channels {
			return false
		}
	}
	return true
}

// startupArgs are the words after the program in a startup command.
func startupArgs(cmd string) []string {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		if filepath.Base(f) == harness.StartupProgram(cmd) {
			return fields[i+1:]
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var (
	sessionIDRe = regexp.MustCompile(`^[0-9A-Za-z-]+$`)
	safeArgRe   = regexp.MustCompile(`^[0-9A-Za-z._:/-]+$`)
)

// transcriptWrite is when Claude last wrote the session's transcript
// (<claudeDir>/projects/<project>/<id>.jsonl), zero when there is none or the
// id is not a plain id.
func transcriptWrite(claudeDir, sessionID string) time.Time {
	if !sessionIDRe.MatchString(sessionID) {
		return time.Time{}
	}
	found, _ := filepath.Glob(filepath.Join(claudeDir, "projects", "*", sessionID+".jsonl"))
	var last time.Time
	for _, f := range found {
		if st, err := os.Stat(f); err == nil && st.ModTime().After(last) {
			last = st.ModTime()
		}
	}
	return last
}

// resumeCommand appends --resume to a command. Every relaunch resumes:
// quietFor only lets through a session whose transcript was found. The id
// is typed into a shell, so anything but a plain id refuses the relaunch.
func resumeCommand(cmd, sessionID string) (string, bool) {
	if !sessionIDRe.MatchString(sessionID) {
		return "", false
	}
	return cmd + " --resume " + sessionID, true
}
