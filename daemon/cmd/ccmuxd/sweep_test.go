package main

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/jobs"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/wtsweep"
)

// The sweep is walled into the repos it sweeps, hooks and config sealed,
// with no network, and opens no folder for a worktree it does not name.
func TestTheSweepIsWalledIntoItsReposWithNoNetwork(t *testing.T) {
	set, host := on(), true
	sb := testSandbox(t, &set, &host)
	repo, _ := gitRepo(t, t.TempDir())
	plan, err := sb.jobWalls(jobs.New{Kind: wtsweep.Kind, Input: wtsweep.Input{Repos: []wtsweep.Repo{{Path: repo}}}})
	if err != nil || plan == nil || !plan.NoNetwork {
		t.Fatalf("walled, no network: %+v %v", plan, err)
	}
	if !slices.Contains(plan.Write, repo) || !slices.Contains(plan.Sealed, filepath.Join(repo, ".git", "hooks")) {
		t.Fatalf("its repo open, its hooks sealed: %v %v", plan.Write, plan.Sealed)
	}
	if slices.Contains(plan.Write, ".") {
		t.Fatalf("no worktree named, no folder opened for one: %v", plan.Write)
	}
}

func TestTheSweepsDayAndHour(t *testing.T) {
	for in, want := range map[string]time.Weekday{"sunday": time.Sunday, "Sun": time.Sunday, "wed": time.Wednesday, "6": time.Saturday} {
		if got, err := parseWeekday(in); err != nil || got != want {
			t.Errorf("parseWeekday(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "7", "su", "funday"} {
		if _, err := parseWeekday(bad); err == nil {
			t.Errorf("parseWeekday(%q) is no day", bad)
		}
	}
	if _, _, err := sweepSchedule("sunday", 24); err == nil {
		t.Error("24 is not an hour")
	}
}

func TestLastSlotIsTheNewestDayAndHourAtOrBeforeNow(t *testing.T) {
	loc := time.Local
	wed := time.Date(2026, 11, 25, 10, 0, 0, 0, loc) // a Wednesday
	for _, c := range []struct {
		now  time.Time
		day  time.Weekday
		hour int
		want time.Time
	}{
		{wed, time.Sunday, 4, time.Date(2026, 11, 22, 4, 0, 0, 0, loc)},
		{wed, time.Wednesday, 4, time.Date(2026, 11, 25, 4, 0, 0, 0, loc)},
		{wed, time.Wednesday, 11, time.Date(2026, 11, 18, 11, 0, 0, 0, loc)}, // later today: last week's
		{wed, time.Thursday, 4, time.Date(2026, 11, 19, 4, 0, 0, 0, loc)},
	} {
		if got := lastSlot(c.now, c.day, c.hour); !got.Equal(c.want) {
			t.Errorf("lastSlot(%v, %v, %d) = %v, want %v", c.now, c.day, c.hour, got, c.want)
		}
	}
}

// The sweep starts once its hour has come, once per week, still within a
// day after it (a daemon down at 04:00), never later, and never when the
// last sweep cannot be read.
func TestTheSweepIsDueOncePerWeek(t *testing.T) {
	now := time.Date(2026, 11, 22, 4, 5, 0, 0, time.Local) // Sunday 04:05
	var last time.Time
	var lastErr error
	started := 0
	wk := &weekly{day: time.Sunday, hour: 4, now: func() time.Time { return now },
		last:  func() (time.Time, error) { return last, lastErr },
		start: func() { started++; last = now }}
	wk.pass()
	wk.pass()
	if started != 1 {
		t.Fatalf("started %d times at its hour, want once", started)
	}
	now = now.Add(7 * 24 * time.Hour)
	lastErr = errors.New("database locked")
	wk.pass()
	if started != 1 {
		t.Fatal("a last sweep that cannot be read starts none")
	}
	lastErr = nil
	now = now.Add(20 * time.Hour) // the daemon was down; still within the day
	wk.pass()
	if started != 2 {
		t.Fatalf("caught up within a day: %d", started)
	}
	fresh := &weekly{day: time.Sunday, hour: 4, now: func() time.Time { return now.Add(7*24*time.Hour + 25*time.Hour) },
		last: func() (time.Time, error) { return time.Time{}, nil }, start: func() { started++ }}
	fresh.pass()
	if started != 2 {
		t.Fatal("more than a day after its hour, the week is skipped")
	}
	other := &weekly{day: time.Sunday, hour: 4, now: func() time.Time { return now },
		last: func() (time.Time, error) { return now.Add(-time.Minute), nil }, start: func() { started++ }}
	other.pass()
	if started != 2 {
		t.Fatal("a sweep the jobs table has for this week is not run again")
	}
}

// Every workspace's repo, once, by its main folder, with the other
// spellings to lock under; role agents' homes and folderless ones left out.
func TestKnownReposAreWorkspacesByMainFolder(t *testing.T) {
	keyOf := func(p string) (string, error) {
		switch p {
		case "/p/app", "/p/app/web", "/p/app/":
			return "/p/app", nil
		case "/p/broken":
			return "", errors.New("git refused")
		}
		return p, nil
	}
	list := []*model.Workspace{
		{RepoPath: "/p/app"}, {RepoPath: "/p/app/web"}, {RepoPath: "/p/app/"},
		{RepoPath: "/p/api"}, {RepoPath: "/agents/radar", Agent: "radar"}, {RepoPath: ""}, {RepoPath: "/p/broken"},
	}
	got := knownRepos(list, keyOf)
	if len(got) != 2 || got[0].Path != "/p/app" || got[1].Path != "/p/api" {
		t.Fatalf("repos = %+v", got)
	}
	if !slices.Equal(got[0].Locks, []string{"/p/app/web"}) || len(got[1].Locks) != 0 {
		t.Fatalf("locks = %+v", got)
	}
}
