package loadout

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// TestEffective is the table of what a repo ends up with: its group's
// on and off, and its own "as the group", on and off, for one entry.
func TestEffective(t *testing.T) {
	on, off := store.ChoiceOn, store.ChoiceOff
	cases := []struct {
		name        string
		group, repo string
		want        bool
	}{
		{"group off, repo as group", "", "", false},
		{"group on, repo as group", on, "", true},
		{"group on, repo off", on, off, false},
		{"group off, repo on", "", on, true},
		{"group on, repo on", on, on, true},
		{"group off, repo off", "", off, false},
	}
	for _, c := range cases {
		group, repo := map[string]string{}, map[string]string{}
		if c.group != "" {
			group["qa"] = c.group
		}
		if c.repo != "" {
			repo["qa"] = c.repo
		}
		if got := slices.Contains(Effective(group, repo), "qa"); got != c.want {
			t.Errorf("%s: on = %v, want %v", c.name, got, c.want)
		}
	}
	got := Effective(map[string]string{"b": on, "a": on, "c": on}, map[string]string{"c": off, "z": on, "a": on})
	if !slices.Equal(got, []string{"a", "b", "z"}) {
		t.Errorf("merged = %v, want each id once, sorted", got)
	}
	if got := Effective(nil, nil); got == nil || len(got) != 0 {
		t.Errorf("nothing chosen = %v, want an empty list", got)
	}
}

// rig is a registry, a catalog and a fake Claude setup on disk.
type rig struct {
	st     *store.SQLite
	agents *agent.Store
	svc    *Service
	claude string
	repos  []Repo
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// plugin makes an installed plugin's folder with one skill.
func plugin(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, "cache", name)
	write(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{"name":"`+name+`","version":"1.0"}`)
	write(t, filepath.Join(dir, "skills", "run", "SKILL.md"), "---\nname: run\n---\n")
	return dir
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := &rig{st: st, agents: agent.NewStore(filepath.Join(root, "ccmux", "agents")), claude: filepath.Join(root, ".claude")}
	installed := map[string]any{}
	for _, name := range []string{"qa", "solo-workflow", "chrome-pool", "kb", "stripe"} {
		installed[name+"@personal"] = []map[string]string{{"installPath": plugin(t, root, name), "lastUpdated": "1"}}
	}
	blob, _ := json.Marshal(map[string]any{"plugins": installed})
	write(t, agent.InstalledPluginsPath(r.claude), string(blob))
	write(t, filepath.Join(r.claude, "skills", "glitchtip", "SKILL.md"), "---\nname: glitchtip\n---\n")
	write(t, filepath.Join(r.claude, "skills", "synced", "x", "SKILL.md"), "x")
	r.svc = &Service{St: st, Catalog: r.agents, ClaudeDir: func() (string, error) { return r.claude, nil },
		Repos: func() []Repo { return r.repos }, Now: func() time.Time { return time.UnixMilli(1000) }}
	return r
}

// repo makes a repo session whose own settings switch plugins on, in a
// window ("" for none).
func (r *rig) repo(t *testing.T, id, window string, plugins ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), id)
	enabled := map[string]bool{}
	for _, p := range plugins {
		enabled[p] = true
	}
	blob, _ := json.Marshal(map[string]any{"enabledPlugins": enabled})
	write(t, filepath.Join(dir, ".claude", "settings.local.json"), string(blob))
	if err := r.st.SaveWorkspace(&model.Workspace{ID: id, Name: id, RepoPath: dir}); err != nil {
		t.Fatal(err)
	}
	if window != "" {
		r.st.CreateWindow(window, "Group "+window) // a second repo finds it there
		if err := r.st.SetWindowMember(id, window); err != nil {
			t.Fatal(err)
		}
	}
	r.repos = append(r.repos, Repo{WorkspaceID: id, Path: dir})
	return dir
}

func (r *rig) set(t *testing.T, scope, id, capID, state string) {
	t.Helper()
	if err := r.st.SetCapabilityChoice(store.CapChoice{Scope: scope, ScopeID: id, CapID: capID, State: state}); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) ids(t *testing.T, wsID string) []string {
	t.Helper()
	snap, err := r.svc.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap.IDs(wsID)
}

// The day-one choices: each group gets what its repos switch on today,
// chrome-pool and the loose skills; a repo in no group gets them itself;
// connectors and everything else stay off.
func TestSeed_GroupsStartWithWhatTheirReposUseToday(t *testing.T) {
	r := newRig(t)
	r.repo(t, "app", "w1", "qa@personal", "solo-workflow@personal")
	r.repo(t, "hq", "w1", "kb@personal", "ghost@nowhere")
	r.repo(t, "lone", "", "qa@personal")
	r.st.CreateWindow("w2", "Empty")
	r.agents.SaveCapability(agent.Capability{ID: "claude.ai-Gmail", Kind: agent.CapConnector, Connector: "claude.ai Gmail"})
	if err := r.svc.Seed(); err != nil {
		t.Fatal(err)
	}
	if got, want := r.ids(t, "app"), []string{"chrome-pool", "glitchtip", "kb", "qa", "solo-workflow"}; !slices.Equal(got, want) {
		t.Errorf("a repo in the group = %v, want %v", got, want)
	}
	if got := r.ids(t, "hq"); !slices.Equal(got, r.ids(t, "app")) {
		t.Errorf("every repo of a group gets the group's list: %v", got)
	}
	if got, want := r.ids(t, "lone"), []string{"chrome-pool", "glitchtip", "qa"}; !slices.Equal(got, want) {
		t.Errorf("a repo in no group = %v, want %v", got, want)
	}
	snap, _ := r.svc.Snapshot()
	if got := snap.Group("w2"); len(got) != 2 || got["chrome-pool"] != store.ChoiceOn || got["glitchtip"] != store.ChoiceOn {
		t.Errorf("a group with no repos = %v, want chrome-pool and the loose skills", got)
	}
}

func TestSeed_RunsOnceAndNeverOverUserChanges(t *testing.T) {
	r := newRig(t)
	r.repo(t, "app", "w1", "qa@personal")
	if err := r.svc.Seed(); err != nil {
		t.Fatal(err)
	}
	r.set(t, store.ChoiceWindow, "w1", "qa", "") // the person switches qa off
	r.repo(t, "late", "w3", "stripe@personal")   // and a new group is made
	if err := r.svc.Seed(); err != nil {
		t.Fatal(err)
	}
	if got := r.ids(t, "app"); slices.Contains(got, "qa") {
		t.Errorf("a second seed switched qa back on: %v", got)
	}
	if got := r.ids(t, "late"); len(got) != 0 {
		t.Errorf("a group made after day one starts with nothing: %v", got)
	}
}

// A start never goes ahead of the day-one choices: it writes them itself
// when the daemon's own seed did not, and fails when it cannot.
func TestForSession_SeedsFirstAndRefusesWhenItCannot(t *testing.T) {
	r := newRig(t)
	app := r.repo(t, "app", "w1", "qa@personal")
	good, _ := os.ReadFile(agent.InstalledPluginsPath(r.claude))
	write(t, agent.InstalledPluginsPath(r.claude), "{ not json")
	sess := store.RunnerSession{ID: "s", Cwd: app, WorkspaceID: "app"}
	if _, _, err := r.svc.ForSession(sess); !errors.Is(err, ErrNotSeeded) {
		t.Fatalf("a start while the seed cannot be written: %v", err)
	}
	if seeded, _ := r.st.CapabilityChoicesSeeded(); seeded {
		t.Fatal("a failed seed left its mark")
	}
	write(t, agent.InstalledPluginsPath(r.claude), string(good))
	if lo := loadoutOf(t, r, sess); !lo.EnabledPlugins["qa@personal"] {
		t.Fatalf("once the cause is fixed the start seeds and loads the repo's plugins: %+v", lo.EnabledPlugins)
	}
}

// A session loads what its own workspace comes to: a helper in another
// repo follows that repo, not its lead's.
func TestForSession_FollowsItsOwnRepo(t *testing.T) {
	r := newRig(t)
	app := r.repo(t, "app", "w1", "qa@personal")
	hq := r.repo(t, "hq", "w1")
	if err := r.svc.Seed(); err != nil {
		t.Fatal(err)
	}
	r.set(t, store.ChoiceWorkspace, "hq", "qa", store.ChoiceOff)
	r.set(t, store.ChoiceWorkspace, "hq", "stripe", store.ChoiceOn)

	lead := loadoutOf(t, r, store.RunnerSession{ID: "lead", Cwd: app, WorkspaceID: "app"})
	if !lead.EnabledPlugins["qa@personal"] || lead.EnabledPlugins["stripe@personal"] || !slices.Contains(lead.Skills, "qa:run") {
		t.Errorf("the lead's loadout: %+v", lead)
	}
	helper := loadoutOf(t, r, store.RunnerSession{ID: "helper", Cwd: hq, WorkspaceID: "hq", Repo: hq})
	if helper.EnabledPlugins["qa@personal"] || !helper.EnabledPlugins["stripe@personal"] {
		t.Errorf("the helper follows hq, not the lead's repo: %+v", helper.EnabledPlugins)
	}
	none := loadoutOf(t, r, store.RunnerSession{ID: "nowhere", Cwd: app})
	if slices.Contains(mapValues(none.EnabledPlugins), true) || len(none.Skills) != 0 || none.Connectors {
		t.Errorf("a session in no workspace loads nothing: %+v", none)
	}
}

func mapValues(m map[string]bool) []bool {
	var out []bool
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func loadoutOf(t *testing.T, r *rig, sess store.RunnerSession) agent.Loadout {
	t.Helper()
	file, _, err := r.svc.ForSession(sess)
	if err != nil {
		t.Fatal(err)
	}
	var lo agent.Loadout
	if err := json.Unmarshal(file, &lo); err != nil {
		t.Fatal(err)
	}
	return lo
}

// A running session is out of date once what is on for its repo, or a
// version of something it loaded, is not what it started with.
func TestStale(t *testing.T) {
	r := newRig(t)
	app := r.repo(t, "app", "w1", "qa@personal")
	if err := r.svc.Seed(); err != nil {
		t.Fatal(err)
	}
	sess := store.RunnerSession{ID: "s", Cwd: app, WorkspaceID: "app", Wanted: store.SessionRunning}
	stale := func() bool {
		t.Helper()
		snap, err := r.svc.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		return snap.Stale(sess)
	}
	if !stale() {
		t.Error("a running session that never recorded a loadout has everything: out of date")
	}
	_, record, err := r.svc.ForSession(sess)
	if err != nil {
		t.Fatal(err)
	}
	sess.Loadout = record
	if rec, ok := Record(sess); !ok || rec.StartedAt != 1000 || len(rec.Entries) != 3 {
		t.Fatalf("record = %+v %v", rec, ok)
	}
	if stale() {
		t.Error("a session just started is up to date")
	}
	r.set(t, store.ChoiceWorkspace, "app", "stripe", store.ChoiceOn)
	if !stale() {
		t.Error("an entry switched on for the repo makes its session out of date")
	}
	r.set(t, store.ChoiceWorkspace, "app", "stripe", "")
	if stale() {
		t.Error("back to the list it started with is up to date again")
	}
	r.set(t, store.ChoiceWorkspace, "other", "stripe", store.ChoiceOn)
	if stale() {
		t.Error("another repo's choice does not touch this session")
	}
	qa, _ := r.agents.SaveCapability(agent.Capability{ID: "qa", Kind: agent.CapPlugin, Path: "/somewhere/else"})
	if qa.Path == "" || !stale() {
		t.Error("a loaded entry that now points elsewhere makes the session out of date")
	}
	sess.Wanted = store.SessionAsleep
	if stale() {
		t.Error("an asleep session reads its list at its next start: never out of date")
	}
}
