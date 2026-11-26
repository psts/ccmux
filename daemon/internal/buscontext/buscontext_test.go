package buscontext

import (
	"strings"
	"testing"
)

var (
	agentReader = Reader{Agent: true}
	testRepos   = []Session{
		{Name: "backend", RepoPath: "/ext/projects/chartlabs/backend", Owns: "API, database, auth", Status: "live"},
		{Name: "hq", RepoPath: "/ext/projects/chartlabs/hq", Status: "cold"},
	}
	testAgents = []Agent{
		{Name: "x-poster", Description: "Posts X threads.", State: "absent"},
		{Name: "kb-writer", Description: "Writes KB articles.", State: "running"},
	}
)

// A role agent reads the repos it reaches: it gets the folders, and what
// each repo owns.
func TestParagraphAndSection(t *testing.T) {
	if Paragraph(agentReader, nil, "", nil) != "" || Section(agentReader, nil, "", nil) != "" {
		t.Fatal("nothing to say: nothing appended")
	}
	p := Paragraph(agentReader, testRepos, "/w/shared", testAgents)
	for _, want := range []string{
		"PROJECT SESSIONS IN THIS WINDOW", "spawn_if_missing=true", "read there if your harness reaches it",
		"- backend: owns API, database, auth; /ext/projects/chartlabs/backend [live]",
		"- hq: /ext/projects/chartlabs/hq [archived; starts on contact]",
		"SHARED FOLDER of this window: /w/shared.", "folder named after you", ".env",
		"AGENTS ON THIS BUS",
		"- x-poster: Posts X threads. [not in this project yet; starts on contact]",
		"- kb-writer: Writes KB articles. [running]",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("paragraph missing %q:\n%s", want, p)
		}
	}
	if !strings.HasPrefix(p, "\n\n") {
		t.Error("paragraph must be separated from whatever it is appended to")
	}
	if strings.Index(p, "PROJECT SESSIONS") > strings.Index(p, "SHARED FOLDER") || strings.Index(p, "SHARED FOLDER") > strings.Index(p, "AGENTS ON THIS BUS") {
		t.Error("order: sessions (where the project is), then the shared folder, then agents")
	}
	s := Section(agentReader, testRepos, "/w/shared", testAgents)
	for _, want := range []string{"Project sessions in this window", "- hq: ", "Shared folder of this window", "/w/shared", "Agents (message by name", "kb-writer"} {
		if !strings.Contains(s, want) {
			t.Errorf("section missing %q:\n%s", want, s)
		}
	}
	// Each half stands alone: a window with repos and no agents, or the reverse.
	if p := Paragraph(agentReader, testRepos, "", nil); !strings.Contains(p, "backend") || strings.Contains(p, "AGENTS ON") || strings.Contains(p, "SHARED") {
		t.Errorf("sessions only: %s", p)
	}
	if p := Paragraph(agentReader, nil, "", testAgents); strings.Contains(p, "PROJECT SESSIONS") || !strings.Contains(p, "x-poster") {
		t.Errorf("agents only: %s", p)
	}
}

// A repo session owns its repo and must not touch another: it is told to
// delegate to the owner and how to answer a delegation, sees what each
// repo owns and itself marked, and is never handed another repo's folder.
func TestParagraphForARepoSession(t *testing.T) {
	r := Reader{Self: "frontend"}
	repos := append([]Session{{Name: "frontend", RepoPath: "/ext/projects/chartlabs/frontend", Owns: "web UI", Status: "live"}}, testRepos...)
	for _, text := range []string{Paragraph(r, repos, "", nil), Section(r, repos, "", nil)} {
		for _, want := range []string{"- frontend (you): owns web UI [live]", "- backend: owns API, database, auth [live]", "- hq [archived; starts on contact]", "delegate"} {
			if !strings.Contains(text, want) {
				t.Errorf("missing %q:\n%s", want, text)
			}
		}
		if strings.Contains(text, "/ext/projects/chartlabs") || strings.Contains(text, "read there") {
			t.Errorf("a repo session must not be pointed at another repo's code:\n%s", text)
		}
	}
	p := Paragraph(r, repos, "", nil)
	for _, want := range []string{"Never read or change another session's repo", "send_message if anything is unclear", "update_task"} {
		if !strings.Contains(p, want) {
			t.Errorf("paragraph missing %q:\n%s", want, p)
		}
	}
}

func TestAMainSessionIsToldItsHelpersAndTasks(t *testing.T) {
	r := Reader{Main: true, Self: "hq"}
	sessions := []Session{{Name: "hq", RepoPath: "/r/hq", Status: "live"}, {Name: "backend", RepoPath: "/r/backend", Status: "cold"}}
	got := Paragraph(r, sessions, "", nil)
	for _, want := range []string{"you are this repo's main session", "helper of your own", "never lands a change", "file it with the tasks tool", "hq (you)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "delegate it to that session") {
		t.Fatalf("a main session is not told to hand work to the repo's own session:\n%s", got)
	}
	if got := Section(r, sessions, "", nil); !strings.Contains(got, "a helper of yours answers for it") {
		t.Fatalf("list_peers says the same:\n%s", got)
	}
	plain := Paragraph(Reader{Self: "hq"}, sessions, "", nil)
	if !strings.Contains(plain, "delegate it to that session") || strings.Contains(plain, "main session") {
		t.Fatalf("every other repo session keeps the owner rule:\n%s", plain)
	}
	for room := len(got) - 1; room > 0; room -= 7 {
		if short := Fit(r, sessions, "", nil, room); !strings.Contains(short, "main session") || strings.Contains(short, "delegate it to that session") {
			t.Fatalf("room %d: every shortened form keeps the main session's rule:\n%s", room, short)
		}
	}
}

// A planner asks other repos through helpers of its own, like a main
// session, and is never told to hand work to a repo's own session: it
// builds nothing, and a delegation would land in a person's conversation.
func TestAPlannerIsToldItsHelpersAndNeverToDelegate(t *testing.T) {
	r := Reader{Planner: true, Self: "hq"}
	sessions := []Session{{Name: "hq", RepoPath: "/r/hq", Status: "live"}, {Name: "backend", RepoPath: "/r/backend", Status: "cold"}}
	got := Paragraph(r, sessions, "", nil)
	for _, want := range []string{"you are this repo's planner", "helper of your own", "never lands a change", "a part of the task you file", "hq (you)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"delegate it to that session", "main session"} {
		if strings.Contains(got, not) {
			t.Fatalf("a planner is told %q:\n%s", not, got)
		}
	}
	if got := Section(r, sessions, "", nil); !strings.Contains(got, "a helper of yours answers for it") {
		t.Fatalf("list_peers says the same:\n%s", got)
	}
	for room := len(got) - 1; room > 0; room -= 7 {
		if short := Fit(r, sessions, "", nil, room); !strings.Contains(short, "this repo's planner") || strings.Contains(short, "delegate it to that session") {
			t.Fatalf("room %d: every shortened form keeps the planner's rule:\n%s", room, short)
		}
	}
}
