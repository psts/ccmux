package taskq

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

func TestPartRoutesGivesEachRowItsOwnNameAndPort(t *testing.T) {
	hs := []model.Hostname{{Name: "chartlabs-thyra", Port: 3011}, {Name: "chartlabs-astrid", Port: 8011}}
	got := PartRoutes(12, 7, hs)
	want := []Route{
		{Name: "chartlabs-thyra-t12", Port: 22070, Main: "chartlabs-thyra", MainPort: 3011},
		{Name: "chartlabs-astrid-t12", Port: 22071, Main: "chartlabs-astrid", MainPort: 8011},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d routes, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("route %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPartRoutesStaysInsideItsRange(t *testing.T) {
	hs := make([]model.Hostname, 12)
	for i := range hs {
		hs[i] = model.Hostname{Name: "a", Port: 1}
	}
	for _, part := range []int64{0, 1, portSlots - 1, portSlots, 999999} {
		rs := PartRoutes(1, part, hs)
		if len(rs) != portStride {
			t.Fatalf("part %d: %d routes, want the %d a block holds", part, len(rs), portStride)
		}
		for _, r := range rs {
			if r.Port < portBase || r.Port >= 32768 || model.IsLegacyAllocatedPort(r.Port) {
				t.Errorf("part %d: port %d outside the task range", part, r.Port)
			}
		}
	}
}

func TestPartRoutesNoHostnamesNoRoutes(t *testing.T) {
	if rs := PartRoutes(3, 3, nil); len(rs) != 0 {
		t.Fatalf("got %+v, want none", rs)
	}
}

func TestTaskHostnameStaysOneLabel(t *testing.T) {
	long := strings.Repeat("a", 57) + "-bcdefg"
	got := TaskHostname(long, 123)
	if len(got) > 63 || !strings.HasSuffix(got, "-t123") || strings.Contains(got, "--") {
		t.Fatalf("TaskHostname = %q (%d chars)", got, len(got))
	}
	if got := TaskHostname("chartlabs-app", 4); got != "chartlabs-app-t4" {
		t.Fatalf("TaskHostname = %q", got)
	}
}

// A feature with an app and a backend part: each worker's brief names its
// own ports and addresses and its teammate's, and while the panes are open
// the devhost server gets every route; once a pane closes, its routes go.
func TestRunner_WorkersGetTheirOwnServers(t *testing.T) {
	g := newRig(t)
	g.host.domain = "dev.test"
	g.host.hostnames = map[string][]model.Hostname{
		"app":     {{Name: "chartlabs-app", Port: 4000}},
		"backend": {{Name: "chartlabs-thor", Port: 8008}},
	}
	g.feature(5, 0, "app", "backend")
	g.r.Tick()
	app, backend := g.partsOf(5)[0], g.partsOf(5)[1]
	brief, _ := os.ReadFile(app.Worktree + ".md")
	for _, want := range []string{
		"chartlabs-app (usually port 4000): yours is port 22010, at https://chartlabs-app-t5.dev.test",
		"chartlabs-thor-t5 (instead of chartlabs-thor): https://chartlabs-thor-t5.dev.test",
		"from /p/app",
		"Do not run migrations",
	} {
		if !strings.Contains(string(brief), want) {
			t.Errorf("app brief lacks %q:\n%s", want, brief)
		}
	}
	g.r.Tick() // routes are sent at the start of a pass, so the next one has them
	want := map[string]int{"chartlabs-app-t5": 22010, "chartlabs-thor-t5": 22020}
	if fmt.Sprint(g.host.routes) != fmt.Sprint(want) {
		t.Fatalf("routes = %v, want %v", g.host.routes, want)
	}
	delete(g.host.panes, backend.PaneID)
	g.r.Tick()
	if _, ok := g.host.routes["chartlabs-thor-t5"]; ok {
		t.Fatalf("a closed worker's route stayed: %v", g.host.routes)
	}
}

// Without a dev domain the brief gives bare ports; with no hostnames at all
// it says so rather than listing nothing.
func TestServersBriefWithoutNames(t *testing.T) {
	g := newRig(t)
	g.host.hostnames = map[string][]model.Hostname{"app": {{Name: "a", Port: 4000}}}
	j := job{task: store.Task{ID: 2}, part: store.Part{ID: 3, RepoPath: "/p/app"}}
	if got, err := g.r.serversBrief(j); err != nil || !strings.Contains(got, "yours is port 22030, at http://127.0.0.1:22030") {
		t.Errorf("no-domain brief (%v):\n%s", err, got)
	}
	j.part.RepoPath = "/p/other"
	if got, err := g.r.serversBrief(j); err != nil || !strings.Contains(got, "maps no dev servers") {
		t.Errorf("no-hostnames brief (%v):\n%s", err, got)
	}
}

// Names that cannot be read are never "no names": a worker's own fail its
// start, a teammate's are left out while the other teammate's stay listed.
func TestServersBriefUnreadableNames(t *testing.T) {
	g := newRig(t)
	g.host.domain = "dev.test"
	g.host.hostnames = map[string][]model.Hostname{
		"app": {{Name: "app", Port: 4000}}, "admin": {{Name: "thyra", Port: 3011}}}
	g.host.nameErr = map[string]error{"backend": errors.New("window list unreadable")}
	j := job{task: store.Task{ID: 2}, part: store.Part{ID: 3, RepoPath: "/p/app"},
		mates: []store.Part{{ID: 4, RepoPath: "/p/backend"}, {ID: 5, RepoPath: "/p/admin"}}}
	got, err := g.r.serversBrief(j)
	if err != nil || strings.Contains(got, "backend part") || !strings.Contains(got, "thyra-t2") {
		t.Fatalf("brief with a teammate's names unreadable (%v):\n%s", err, got)
	}
	j.part.RepoPath = "/p/backend"
	if _, err := g.r.serversBrief(j); err == nil {
		t.Fatal("own names unreadable must fail the start, not brief \"no ports\"")
	}
}

// A pass that cannot read a live worker's names sends nothing, so the last
// good routes stay up; the next good pass replaces them.
func TestPublishRoutesKeepsLastGoodRoutes(t *testing.T) {
	g := newRig(t)
	g.host.hostnames = map[string][]model.Hostname{"app": {{Name: "app", Port: 4000}}}
	g.host.panes["pane-1"] = PaneState{}
	tasks := []store.Task{{ID: 6}}
	parts := []store.Part{{ID: 1, TaskID: 6, RepoPath: "/p/app", PaneID: "pane-1"}}
	g.r.publishRoutes(tasks, parts)
	g.host.nameErr = map[string]error{"app": errors.New("window list unreadable")}
	g.r.publishRoutes(tasks, parts)
	if g.host.routes["app-t6"] != 22010 {
		t.Fatalf("a failed read dropped live routes: %v", g.host.routes)
	}
	g.host.nameErr = nil
	delete(g.host.panes, "pane-1")
	g.r.publishRoutes(tasks, parts)
	if len(g.host.routes) != 0 {
		t.Fatalf("a closed worker's route stayed: %v", g.host.routes)
	}
}
