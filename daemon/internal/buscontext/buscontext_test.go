package buscontext

import (
	"strings"
	"testing"
)

func TestParagraphAndSection(t *testing.T) {
	if Paragraph(nil, "", nil) != "" || Section(nil, "", nil) != "" {
		t.Fatal("nothing to say: nothing appended")
	}
	sessions := []Session{
		{Name: "backend", RepoPath: "/ext/projects/chartlabs/backend", Status: "live"},
		{Name: "hq", RepoPath: "/ext/projects/chartlabs/hq", Status: "cold"},
	}
	agents := []Agent{
		{Name: "x-poster", Description: "Posts X threads.", State: "absent"},
		{Name: "kb-writer", Description: "Writes KB articles.", State: "running"},
	}
	p := Paragraph(sessions, "/w/shared", agents)
	for _, want := range []string{
		"PROJECT SESSIONS IN THIS WINDOW", "spawn_if_missing=true",
		"- backend: /ext/projects/chartlabs/backend [live]",
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
	s := Section(sessions, "/w/shared", agents)
	for _, want := range []string{"Project sessions in this window", "- hq: ", "Shared folder of this window", "/w/shared", "Agents (message by name", "kb-writer"} {
		if !strings.Contains(s, want) {
			t.Errorf("section missing %q:\n%s", want, s)
		}
	}
	// Each half stands alone: a window with repos and no agents, or the reverse.
	if p := Paragraph(sessions, "", nil); !strings.Contains(p, "backend") || strings.Contains(p, "AGENTS ON") || strings.Contains(p, "SHARED") {
		t.Errorf("sessions only: %s", p)
	}
	if p := Paragraph(nil, "", agents); strings.Contains(p, "PROJECT SESSIONS") || !strings.Contains(p, "x-poster") {
		t.Errorf("agents only: %s", p)
	}
}
