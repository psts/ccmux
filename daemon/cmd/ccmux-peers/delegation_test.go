package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFirstLine_RuneSafeTruncation(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"first\nsecond", 10, "first"},
		{"abcdef", 5, "abcde…"},
		// Multi-byte runes must not be split: 6 runes of 2–3 bytes each.
		{"éééééé", 5, "ééééé…"},
		{"—dash—heavy—prose—here", 10, "—dash—heav…"},
	}
	for _, c := range cases {
		got := firstLine(c.in, c.max)
		if got != c.want {
			t.Errorf("firstLine(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("firstLine(%q, %d) produced a broken rune: %q", c.in, c.max, got)
		}
	}
}

func TestTaskLine_RolesAndPendingWorker(t *testing.T) {
	self := "me111111"
	out := openTask{TaskID: "tsk_a", FromID: self, ToID: "w1", Status: "working",
		StatusMessage: "halfway", Text: "do the thing\ndetails below"}
	if got := taskLine(out, self); got != "tsk_a [working] delegated by you to w1 — halfway: do the thing" {
		t.Errorf("outbound line = %q", got)
	}

	in := openTask{TaskID: "tsk_b", FromID: "d1", ToID: self, Status: "sent", Text: "incoming work"}
	if got := taskLine(in, self); got != "tsk_b [sent] yours to do, from d1: incoming work" {
		t.Errorf("inbound line = %q", got)
	}

	pending := openTask{TaskID: "tsk_c", FromID: self, ToID: "", Status: "sent", Text: "spawned"}
	if got := taskLine(pending, self); !strings.Contains(got, "(worker still starting)") {
		t.Errorf("pending-worker line = %q", got)
	}
}
