package taskq

// A builder's "For you" lines: what a person must check before the work
// ships, and why. The builder decides, at the end of its work, from a
// strict list (the brief's briefForYou). Most tasks have none, and only a
// task with lines a person has not read shows the mark.

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"ccmux.dev/ccmuxd/internal/store"
)

const (
	// ForYouMax is how many lines a report may carry.
	ForYouMax = 3
	// ForYouLineMax is how long one line may be, in characters.
	ForYouLineMax = 140
)

// ForYouLines checks a report's lines: each trimmed and blank ones
// dropped, then at most ForYouMax, each one line of at most ForYouLineMax
// characters. nil stays nil (the report did not say), so a report without
// lines keeps the ones an earlier report gave; an empty list clears them.
func ForYouLines(in []string) ([]string, error) {
	if in == nil {
		return nil, nil
	}
	out := []string{}
	for _, line := range in {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.ContainsAny(line, "\r\n") {
			return nil, fmt.Errorf("read: %q is more than one line; give each thing to check as its own short line", line)
		}
		if n := utf8.RuneCountInString(line); n > ForYouLineMax {
			return nil, fmt.Errorf("read: a line is %d characters, at most %d: shorten it to what to check and why", n, ForYouLineMax)
		}
		out = append(out, line)
	}
	if len(out) > ForYouMax {
		return nil, fmt.Errorf("read: %d lines, at most %d: keep only what a person must check (data changed or deleted, access, money, anything sent to people, security)", len(out), ForYouMax)
	}
	return out, nil
}

// Marked says whether task t shows the mark: a part of it (in parts, all
// parts or only its own) has For-you lines, and a person has not marked
// them read.
func Marked(t store.Task, parts []store.Part) bool {
	return !t.ForYouRead && len(ForYouOf(t.ID, parts)) > 0
}

// ForYouOf is task id's For-you lines, its parts' in order. A task of
// several parts names the repo on each line.
func ForYouOf(id int64, parts []store.Part) []string {
	mine := PartsOf(parts, id)
	var out []string
	for _, p := range mine {
		for _, line := range p.ForYou {
			if len(mine) > 1 {
				line = filepath.Base(p.RepoPath) + ": " + line
			}
			out = append(out, line)
		}
	}
	return out
}
