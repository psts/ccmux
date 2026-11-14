package buscontext

import (
	"fmt"
	"strings"
	"testing"
)

// fitWindow is a window like the ones in use: six repos that each say what
// they own, a shared folder, and six role agents with a sentence each.
func fitWindow() ([]Session, string, []Agent) {
	var sessions []Session
	for i := 0; i < 6; i++ {
		sessions = append(sessions, Session{
			Name: fmt.Sprintf("repo-%d", i), RepoPath: fmt.Sprintf("/r/repo-%d", i), Status: "cold",
			Owns: fmt.Sprintf("thing %d and its API, its storage, the alerts it raises and the page that shows them all", i),
		})
	}
	var agents []Agent
	for i := 0; i < 6; i++ {
		agents = append(agents, Agent{Name: fmt.Sprintf("agent-%d", i), State: "absent",
			Description: "Reads the project's outside sources each weekday morning and files what it finds."})
	}
	return sessions, "/w/shared", agents
}

const (
	repoRule  = "Never read or change another session's repo"
	ownsWhole = "and the page that shows them all ["
)

// What fits is sent as Paragraph has it: Fit changes nothing for a window
// small enough, and nothing at all for a window with nothing in it.
func TestFitIsTheParagraphWhenItFits(t *testing.T) {
	r := Reader{Self: "repo-2"}
	sessions, shared, agents := fitWindow()
	whole := Paragraph(r, sessions, shared, agents)
	for _, room := range []int{len(whole), len(whole) + 1, 1 << 20} {
		if got := Fit(r, sessions, shared, agents, room); got != whole {
			t.Fatalf("room %d holds the %d-byte paragraph, yet it was changed:\n%s", room, len(whole), got)
		}
	}
	for _, room := range []int{-1, 0, 4096} {
		if got := Fit(r, nil, "", nil, room); got != "" {
			t.Fatalf("an empty window said %q at room %d", got, room)
		}
	}
}

// Each step down gives up the least useful thing still left: what each
// agent does, then the agents' names, then the end of every owns line a
// little at a time, then the owns lines, then the sessions' names.
func TestFitGivesUpTheLeastUsefulFirst(t *testing.T) {
	r := Reader{Self: "repo-2"}
	sessions, shared, agents := fitWindow()
	steps := []struct {
		name        string
		has, hasNot []string
	}{
		{"agents by name", []string{ownsWhole, "agent-0, agent-1", "agent-5"}, []string{"Reads the project's", "…"}},
		{"agents counted", []string{ownsWhole, "AGENTS ON THIS BUS: 6"}, []string{"agent-0", "…"}},
		{"owns lines cut", []string{"- repo-2 (you): owns thing 2 and its API", "…", "list_peers has each line whole"}, []string{ownsWhole, "agent-0"}},
		{"owns lines cut shorter", []string{"- repo-2 (you): owns thing 2", "…"}, []string{"the alerts it raises"}},
		{"owns lines cut to a hint", []string{"- repo-2 (you): owns thing 2", "…"}, []string{"its storage"}},
		{"names only", []string{"repo-0, repo-1, repo-2 (you), repo-3", "says what each owns", "agent-5"}, []string{"owns thing", "…"}},
		{"counted", []string{"Sessions: 6", "AGENTS ON THIS BUS: 6"}, []string{"repo-1", "agent-0"}},
	}
	room := len(Paragraph(r, sessions, shared, agents)) - 1
	for _, st := range steps {
		got := Fit(r, sessions, shared, agents, room)
		if len(got) > room {
			t.Fatalf("%s: %d bytes for a room of %d:\n%s", st.name, len(got), room, got)
		}
		for _, want := range append(st.has, repoRule, "SHARED FOLDER of this window: /w/shared.", "list_peers") {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing %q:\n%s", st.name, want, got)
			}
		}
		for _, not := range st.hasNot {
			if strings.Contains(got, not) {
				t.Errorf("%s: still has %q:\n%s", st.name, not, got)
			}
		}
		room = len(got) - 1
	}
}

// Whatever the room, the text is whole lines under the room, with the
// reader's rule and the way to the rest; only when not even the shortest
// form fits is it over, and then it is that shortest form.
func TestFitNeverCutsAndNeverDropsTheRule(t *testing.T) {
	r := Reader{Self: "repo-2"}
	sessions, shared, agents := fitWindow()
	whole := Paragraph(r, sessions, shared, agents)
	shortest := Fit(r, sessions, shared, agents, 0)
	if len(shortest) >= len(whole)/3 {
		t.Fatalf("the shortest form is %d of the paragraph's %d bytes: not much of a floor", len(shortest), len(whole))
	}
	for room := len(whole); room >= 0; room -= 7 {
		got := Fit(r, sessions, shared, agents, room)
		if len(got) > room && got != shortest {
			t.Fatalf("room %d: %d bytes, and not the shortest form", room, len(got))
		}
		if !strings.Contains(got, repoRule) || !strings.HasPrefix(got, "\n\n") || !strings.HasSuffix(got, "\n") {
			t.Fatalf("room %d: the rule, the separator or the last line is gone:\n%s", room, got)
		}
		if got != whole && !strings.Contains(got, "list_peers") {
			t.Fatalf("room %d: shortened without saying where the rest is:\n%s", room, got)
		}
	}
}

// A role agent reads the repos it reaches. While the lines are there it
// keeps the folders; once they are not, it is sent to list_peers for them
// instead of being told about folders it was not given.
func TestFitForARoleAgent(t *testing.T) {
	r := Reader{Agent: true}
	sessions, shared, agents := fitWindow()
	seen := map[string]bool{}
	for room := len(Paragraph(r, sessions, shared, agents)) - 1; room >= 0; room -= 5 {
		got := Fit(r, sessions, shared, agents, room)
		folders, toldOfFolders := strings.Contains(got, "/r/repo-0"), strings.Contains(got, "at these folders")
		if folders != toldOfFolders {
			t.Fatalf("room %d: folders listed %v, yet told of them %v:\n%s", room, folders, toldOfFolders, got)
		}
		if !folders && !strings.Contains(got, "list_peers shows each one's folder") {
			t.Fatalf("room %d: no folders and no way to them:\n%s", room, got)
		}
		if strings.Contains(got, repoRule) {
			t.Fatalf("room %d: a role agent got the repo session's rule:\n%s", room, got)
		}
		seen[fmt.Sprint(folders)] = true
	}
	if len(seen) != 2 {
		t.Fatalf("the walk must pass forms with and without folders, saw %v", seen)
	}
}

// Each part stands alone, as in Paragraph: a window with no agents says
// nothing of agents, one with no shared folder nothing of that, and one
// with agents only still names them.
func TestFitPartsStandAlone(t *testing.T) {
	r := Reader{Self: "repo-2"}
	sessions, shared, agents := fitWindow()
	if got := Fit(r, sessions, "", nil, 700); !strings.Contains(got, "repo-5") || strings.Contains(got, "AGENTS") || strings.Contains(got, "SHARED") {
		t.Errorf("sessions only:\n%s", got)
	}
	if got := Fit(r, nil, shared, agents, 400); strings.Contains(got, "PROJECT SESSIONS") || !strings.Contains(got, "agent-5") || !strings.Contains(got, "/w/shared") {
		t.Errorf("no sessions:\n%s", got)
	}
	one := []Agent{{Name: "solo", Description: strings.Repeat("long ", 80), State: "absent"}}
	named := Fit(r, nil, "", one, len(Paragraph(r, nil, "", one))-1)
	if got := Fit(r, nil, "", one, len(named)-1); !strings.Contains(named, "Names: solo.") || !strings.Contains(got, "AGENTS ON THIS BUS: 1 (") {
		t.Errorf("one agent, named then counted:\n%s\n%s", named, got)
	}
}

// A cut owns line keeps whole characters and never ends on a separator.
func TestClip(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"Kök, frys och skafferi", 5, "Kök…"},
		{"alerts, home mode", 8, "alerts…"},
	} {
		if got := clip(c.in, c.n); got != c.want {
			t.Errorf("clip(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}
