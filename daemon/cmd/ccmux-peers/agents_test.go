package main

import (
	"strings"
	"testing"
)

func TestAgentsParagraphAndSection(t *testing.T) {
	if agentsParagraph(nil) != "" || agentsSection(nil) != "" {
		t.Fatal("no agents: nothing appended")
	}
	list := []agentEntry{
		{Name: "x-poster", Description: "Posts X threads.", State: "absent"},
		{Name: "kb-writer", Description: "Writes KB articles.", State: "running"},
	}
	p := agentsParagraph(list)
	for _, want := range []string{"AGENTS ON THIS BUS", "spawn_if_missing=true", "- x-poster: Posts X threads. [not in this project yet; starts on contact]", "- kb-writer: Writes KB articles. [running]"} {
		if !strings.Contains(p, want) {
			t.Errorf("paragraph missing %q:\n%s", want, p)
		}
	}
	if !strings.HasPrefix(p, "\n\n") {
		t.Error("paragraph must be separated from the frozen instructions")
	}
	if s := agentsSection(list); !strings.Contains(s, "Agents (message by name") || !strings.Contains(s, "kb-writer") {
		t.Errorf("section: %s", s)
	}
}
