// Package todos is the rules of the per-repo to-do lists: what an item may
// be, when two sightings are the same item, and what each change does to
// one. A list holds what a session noticed and did not handle right away
// (an "also found", a review finding it left, a follow-up), added by the
// agents themselves through the peers `todo` tool and worked by a person on
// the thread page. It is not a session's own step list (Claude's
// TodoWrite). The rows live in internal/store; internal/api serves them.
package todos

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"ccmux.dev/ccmuxd/internal/store"
)

// An item's statuses: open, planning (in its repo's planning line: handed
// to the repo's planner, which has not filed it yet), tasked (filed as a
// task; before the planning line there was, also handed to the repo's
// session to file), and done.
const (
	Open     = "open"
	Planning = "planning"
	Tasked   = "tasked"
	Done     = "done"
)

// The urgencies, as the tool and the page name them.
const (
	High   = "high"
	Medium = "medium"
	Low    = "low"
)

var ranks = map[string]int{High: 3, Medium: 2, Low: 1}

// Rank orders urgencies, high first; 0 for a word that is not one.
func Rank(urgency string) int { return ranks[urgency] }

// Problem is a change the rules refuse, with why, in words for whoever
// asked. Kind says what sort of refusal it is.
type Problem struct {
	Kind int
	Msg  string
}

func (p *Problem) Error() string { return p.Msg }

// The kinds of refusal: what was asked makes no sense (Invalid), there is no
// such item (Missing), or the list as it stands does not allow it (Conflict).
const (
	Invalid = iota
	Missing
	Conflict
)

func problem(kind int, format string, args ...any) error {
	return &Problem{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// MaxText bounds an item: it is one line on a list, not a plan.
const MaxText = 600

// CheckText trims an item's text and says why it cannot be one.
func CheckText(text string) (string, error) {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "", problem(Invalid, "a to-do item needs text: one line on what is wrong or left to do")
	}
	if n := utf8.RuneCountInString(text); n > MaxText {
		return "", problem(Invalid, "a to-do item is one line, at most %d characters (this one is %d): say what it is, and put the detail where the work happens", MaxText, n)
	}
	return text, nil
}

// CheckUrgency is an urgency as it is kept: medium when none was given.
func CheckUrgency(urgency string) (string, error) {
	urgency = strings.ToLower(strings.TrimSpace(urgency))
	if urgency == "" {
		return Medium, nil
	}
	if Rank(urgency) == 0 {
		return "", problem(Invalid, "urgency is high, medium or low, not %q", urgency)
	}
	return urgency, nil
}

// Normalize is an item's text as two sightings are compared: letters and
// digits only, lower case, single spaces. Agents spot the same thing again
// and again and never word it the same way twice; case, punctuation and
// spacing are the differences this takes away. The digits stay: "x.go:12"
// and "x.go:40" are two places.
func Normalize(text string) string {
	keep := func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}
	if norm := strings.Join(strings.Fields(strings.Map(keep, text)), " "); norm != "" {
		return norm
	}
	return strings.ToLower(strings.TrimSpace(text)) // all punctuation: compared as written
}

// Sort puts a list in the order it is read: what is not done first, high
// urgency before medium before low and the newest first within each, then
// the done ones, the latest done first.
func Sort(list []store.Todo) {
	sort.SliceStable(list, func(i, j int) bool { return before(list[i], list[j]) })
}

func before(a, b store.Todo) bool {
	if (a.Status == Done) != (b.Status == Done) {
		return b.Status == Done
	}
	if a.Status == Done {
		return a.DoneAt > b.DoneAt
	}
	if Rank(a.Urgency) != Rank(b.Urgency) {
		return Rank(a.Urgency) > Rank(b.Urgency)
	}
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	return a.ID > b.ID
}

// Unfinished is the items of list that are not done, in the list's order.
func Unfinished(list []store.Todo) []store.Todo {
	out := []store.Todo{}
	for _, t := range list {
		if t.Status != Done {
			out = append(out, t)
		}
	}
	return out
}
