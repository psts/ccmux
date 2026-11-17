package store

import (
	"path/filepath"
	"slices"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

func choiceStore(t *testing.T) *SQLite {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestCapabilityChoices_SetReadRemove(t *testing.T) {
	st := choiceStore(t)
	set := func(scope, id, capID, state string) {
		t.Helper()
		if err := st.SetCapabilityChoice(CapChoice{Scope: scope, ScopeID: id, CapID: capID, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	set(ChoiceWindow, "w1", "qa", ChoiceOn)
	set(ChoiceWindow, "w1", "kb", ChoiceOn)
	set(ChoiceWorkspace, "ws1", "qa", ChoiceOff)
	set(ChoiceWorkspace, "ws1", "qa", ChoiceOn) // a second write replaces the first
	set(ChoiceWindow, "w1", "kb", "")           // "" removes
	win, err := st.CapabilityChoicesOf(ChoiceWindow, "w1")
	if err != nil || len(win) != 1 || win["qa"] != ChoiceOn {
		t.Fatalf("window choices = %v %v", win, err)
	}
	repo, _ := st.CapabilityChoicesOf(ChoiceWorkspace, "ws1")
	if len(repo) != 1 || repo["qa"] != ChoiceOn {
		t.Fatalf("workspace choices = %v", repo)
	}
	if none, err := st.CapabilityChoicesOf(ChoiceWindow, "nobody"); err != nil || len(none) != 0 {
		t.Fatalf("no rows is no choices: %v %v", none, err)
	}
	all, _ := st.CapabilityChoices()
	if len(all) != 2 || all[0].Scope != ChoiceWindow || all[1].Scope != ChoiceWorkspace {
		t.Fatalf("all choices = %+v", all)
	}
}

func TestCapabilityChoiceUsers_NamesGroupsAndRepos(t *testing.T) {
	st := choiceStore(t)
	if err := st.CreateWindow("w1", "ChartLabs"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveWorkspace(&model.Workspace{ID: "ws1", Name: "app", RepoPath: "/r/app"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []CapChoice{
		{ChoiceWindow, "w1", "qa", ChoiceOn}, {ChoiceWorkspace, "ws1", "qa", ChoiceOn},
		{ChoiceWorkspace, "ws-elsewhere", "qa", ChoiceOn}, {ChoiceWorkspace, "ws2", "qa", ChoiceOff},
	} {
		if err := st.SetCapabilityChoice(c); err != nil {
			t.Fatal(err)
		}
	}
	users, err := st.CapabilityChoiceUsers("qa")
	if want := []string{"group ChartLabs", "repo app", "repo ws-elsewhere"}; err != nil || !slices.Equal(users, want) {
		t.Fatalf("users = %v %v, want %v (an off is not a use)", users, err, want)
	}
	if none, _ := st.CapabilityChoiceUsers("kb"); len(none) != 0 {
		t.Fatalf("an entry nobody chose has users: %v", none)
	}
}

// The day-one choices are written once: a second run, after people changed
// things, writes nothing.
func TestSeedCapabilityChoices_OnceAndNeverOverChanges(t *testing.T) {
	st := choiceStore(t)
	if seeded, _ := st.CapabilityChoicesSeeded(); seeded {
		t.Fatal("a new registry is not seeded")
	}
	rows := []CapChoice{{ChoiceWindow, "w1", "qa", ChoiceOn}, {ChoiceWindow, "w1", "chrome-pool", ChoiceOn}}
	did, err := st.SeedCapabilityChoices(rows, 5)
	if err != nil || !did {
		t.Fatalf("first seed: %v %v", did, err)
	}
	if seeded, _ := st.CapabilityChoicesSeeded(); !seeded {
		t.Fatal("the seed left no mark")
	}
	// The person switches chrome-pool off, then the daemon restarts.
	st.SetCapabilityChoice(CapChoice{Scope: ChoiceWindow, ScopeID: "w1", CapID: "chrome-pool"})
	did, err = st.SeedCapabilityChoices(append(rows, CapChoice{ChoiceWindow, "w2", "qa", ChoiceOn}), 9)
	if err != nil || did {
		t.Fatalf("second seed wrote: %v %v", did, err)
	}
	all, _ := st.CapabilityChoices()
	if len(all) != 1 || all[0].CapID != "qa" || all[0].ScopeID != "w1" {
		t.Fatalf("a second seed changed the choices: %+v", all)
	}
}

func TestSeedCapabilityChoices_EmptySeedStillMarks(t *testing.T) {
	st := choiceStore(t)
	if did, err := st.SeedCapabilityChoices(nil, 1); err != nil || !did {
		t.Fatalf("seed: %v %v", did, err)
	}
	if seeded, _ := st.CapabilityChoicesSeeded(); !seeded {
		t.Fatal("a host with nothing to seed is still seeded: later groups start empty")
	}
}

func TestCapabilityChoices_GoWithTheirWindowAndWorkspace(t *testing.T) {
	st := choiceStore(t)
	st.CreateWindow("w1", "ChartLabs")
	st.SaveWorkspace(&model.Workspace{ID: "ws1", Name: "app"})
	st.SetCapabilityChoice(CapChoice{ChoiceWindow, "w1", "qa", ChoiceOn})
	st.SetCapabilityChoice(CapChoice{ChoiceWorkspace, "ws1", "qa", ChoiceOff})
	st.SetCapabilityChoice(CapChoice{ChoiceWindow, "w2", "qa", ChoiceOn})
	if err := st.DeleteWindow("w1"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteWorkspace("ws1"); err != nil {
		t.Fatal(err)
	}
	all, _ := st.CapabilityChoices()
	if len(all) != 1 || all[0].ScopeID != "w2" {
		t.Fatalf("choices left behind: %+v", all)
	}
}

func TestRunnerSessionLoadout_KeptOnTheRow(t *testing.T) {
	st := choiceStore(t)
	if err := st.AddRunnerSession(RunnerSession{ID: "s1", Cwd: "/r", Wanted: SessionRunning, UpdatedAt: 7}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunnerSessionLoadout("s1", `{"signature":"abc"}`); err != nil {
		t.Fatal(err)
	}
	got, _ := st.RunnerSession("s1")
	if got.Loadout != `{"signature":"abc"}` || got.UpdatedAt != 7 {
		t.Fatalf("row = %+v", got)
	}
	if err := st.SetRunnerSessionLoadout("nobody", "x"); err != ErrNoSession {
		t.Fatalf("a session with no row: %v", err)
	}
}
