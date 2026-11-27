package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// refreshRig is a rig whose sessions refresh after an hour, with each
// unit's start time set by the test.
func refreshRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.sup.cfg.RefreshAfter = time.Hour
	r.units.since = map[string]time.Time{}
	return r
}

// age makes a session's unit look started d ago.
func (r *rig) age(id string, d time.Duration) {
	r.units.mu.Lock()
	r.units.since[r.paths.Unit(id)] = r.clock.at.Add(-d)
	r.units.mu.Unlock()
}

func (r *rig) restarts(id string) int { return r.units.count("restart " + r.paths.Unit(id)) }

// A session that has run past the age, idle with a cold cache, is started
// again once and says why; a young one is left alone.
func TestAnOldIdleSessionIsStartedAgainOnce(t *testing.T) {
	r := refreshRig(t)
	old, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	young, _ := r.sup.Create(NewSession{Cwd: "/r/web"})
	r.reconcile()
	r.age(old.ID, 2*time.Hour)
	r.age(young.ID, 10*time.Minute)
	r.run[old.ID].snap = map[string]any{"result": map[string]any{"endedAt": r.clock.at.Add(-2 * time.Hour).UnixMilli(), "cacheTtlMs": 3600000}}
	r.reconcile()
	if r.restarts(old.ID) != 1 || r.restarts(young.ID) != 0 {
		t.Fatalf("restarts old %d young %d: %v", r.restarts(old.ID), r.restarts(young.ID), r.units.calls)
	}
	row, _ := r.st.RunnerSession(old.ID)
	if !strings.Contains(row.Note, "to pick up updates") {
		t.Fatalf("its note says why: %q", row.Note)
	}
	r.age(old.ID, time.Minute) // systemd: the restart began a new run
	r.reconcile()
	if r.restarts(old.ID) != 1 {
		t.Fatalf("restarted again: %v", r.units.calls)
	}
}

// One at a time: while the session just restarted has not answered, no
// other is restarted; once it answers, the next one goes.
func TestRefreshesGoOneAtATime(t *testing.T) {
	r := refreshRig(t)
	a, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	b, _ := r.sup.Create(NewSession{Cwd: "/r/web"})
	r.reconcile()
	r.age(a.ID, 2*time.Hour)
	r.age(b.ID, 2*time.Hour)
	r.reconcile()
	if got := r.restarts(a.ID) + r.restarts(b.ID); got != 1 {
		t.Fatalf("restarts in one pass = %d, want 1: %v", got, r.units.calls)
	}
	first, other := a.ID, b.ID
	if r.restarts(b.ID) == 1 {
		first, other = b.ID, a.ID
	}
	r.age(first, time.Minute)
	r.run[first].srv.Close() // its new runner is not up yet
	r.reconcile()
	if r.restarts(other) != 0 {
		t.Fatalf("the next one went before the first answered: %v", r.units.calls)
	}
	r.bootRunner(r.paths.Unit(first))
	r.reconcile()
	if r.restarts(other) != 1 {
		t.Fatalf("the next one did not go once the first answered: %v", r.units.calls)
	}
}

// Nothing a session is doing is cut for a refresh: a turn, a background
// task, a card, a warm cache. And a role agent or a task's builder is
// never refreshed here.
func TestARefreshWaitsForWhatTheSessionIsDoing(t *testing.T) {
	warm := map[string]any{"result": map[string]any{"endedAt": time.Now().UnixMilli(), "cacheTtlMs": 3600000}}
	for name, c := range map[string]struct {
		n        NewSession
		busy     bool
		turnOpen bool
		snap     map[string]any
	}{
		"in a turn":       {n: NewSession{Cwd: "/r/app"}, busy: true},
		"a turn open":     {n: NewSession{Cwd: "/r/app"}, turnOpen: true},
		"background task": {n: NewSession{Cwd: "/r/app"}, snap: map[string]any{"info": map[string]any{"tasks": []any{map[string]any{"id": "t1"}}}}},
		"a card":          {n: NewSession{Cwd: "/r/app"}, snap: map[string]any{"questions": []any{map[string]any{"id": "q1"}}}},
		"warm cache":      {n: NewSession{Cwd: "/r/app"}, snap: warm},
	} {
		t.Run(name, func(t *testing.T) {
			r := refreshRig(t)
			r.clock.at = time.Now()
			sess, err := r.sup.Create(c.n)
			if err != nil {
				t.Fatal(err)
			}
			r.reconcile()
			r.age(sess.ID, 2*time.Hour)
			if fr := r.run[sess.ID]; fr != nil { // an agent does not start on this rig
				fr.busy, fr.turnOpen, fr.snap = c.busy, c.turnOpen, c.snap
			}
			r.reconcile()
			if n := r.restarts(sess.ID); n != 0 {
				t.Fatalf("restarted %d times: %v", n, r.units.calls)
			}
		})
	}
}

// Off unless asked for.
func TestNoRefreshWithoutAnAge(t *testing.T) {
	r := refreshRig(t)
	r.sup.cfg.RefreshAfter = 0
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.age(sess.ID, 48*time.Hour)
	r.reconcile()
	if r.restarts(sess.ID) != 0 {
		t.Fatalf("refreshed with no age set: %v", r.units.calls)
	}
}

func TestSnapshotHold(t *testing.T) {
	now := time.UnixMilli(10_000_000)
	for body, want := range map[string]string{
		`{}`: "", // no turn since it started: no cache to lose
		`{"result":{"endedAt":1000,"cacheTtlMs":3600000}}`:    "",
		`{"result":{"endedAt":9999000,"cacheTtlMs":3600000}}`: "cache warm",
		`{"result":{"endedAt":9900000}}`:                      "cache warm", // no ttl said: five minutes
		`{"result":{"endedAt":9000000}}`:                      "",
		`{"info":{"tasks":[{"id":"t"}]}}`:                     "background task running",
		`{"permissions":[{"id":"p"}]}`:                        "a card is open",
		`not json`:                                            "snapshot unreadable",
	} {
		if got := snapshotHold(json.RawMessage(body), now); got != want {
			t.Errorf("%s: %q, want %q", body, got, want)
		}
	}
}

func TestUnitSince(t *testing.T) {
	if got := unitSince("ActiveEnterTimestamp=@1795269745\n"); !got.Equal(time.Unix(1795269745, 0)) {
		t.Fatalf("got %v", got)
	}
	for _, out := range []string{"ActiveEnterTimestamp=\n", "ActiveEnterTimestamp=n/a", ""} {
		if !unitSince(out).IsZero() {
			t.Errorf("%q: not zero", out)
		}
	}
}

// A role agent and a task's builder are never due here, however old.
func TestOnlyAPersonsSessionIsDue(t *testing.T) {
	r := refreshRig(t)
	r.age("x", 48*time.Hour)
	for _, sess := range []store.RunnerSession{
		{ID: "x", Wanted: store.SessionRunning, Kind: store.SessionAgent},
		{ID: "x", Wanted: store.SessionRunning, Role: store.RoleBuilder},
		{ID: "x", Wanted: store.SessionAsleep},
	} {
		if r.sup.refreshDue(sess, nil) != "" {
			t.Errorf("due: %+v", sess)
		}
	}
	if r.sup.refreshDue(store.RunnerSession{ID: "x", Wanted: store.SessionRunning, Kind: store.SessionLead}, nil) == "" {
		t.Fatal("an old lead is due")
	}
}

// A restarted session that never comes back stops the refreshes there, and
// says so in its note. Parked asleep for failing to start (what a broken
// update does) it still holds them; ended, it lets them go on.
func TestARefreshThatNeverAnswersStopsTheOthers(t *testing.T) {
	r := refreshRig(t)
	a, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	b, _ := r.sup.Create(NewSession{Cwd: "/r/web"})
	r.reconcile()
	r.age(a.ID, 2*time.Hour)
	r.age(b.ID, 2*time.Hour)
	r.reconcile()
	first, other := a.ID, b.ID
	if r.restarts(b.ID) == 1 {
		first, other = b.ID, a.ID
	}
	r.age(first, time.Minute)
	r.run[first].srv.Close() // broken: it never answers again
	r.units.onStart = nil    // and a start does not bring it back
	r.clock.jump(refreshWait + time.Minute)
	r.reconcile()
	r.reconcile()
	if r.restarts(other) != 0 {
		t.Fatalf("the next one went after the first never answered: %v", r.units.calls)
	}
	row, _ := r.st.RunnerSession(first)
	if !strings.Contains(row.Note, "no other session is restarted for updates") {
		t.Fatalf("its note says the refreshes stopped: %q", row.Note)
	}
	if err := r.sup.park(first, "put to sleep: it failed to start 5 times"); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if r.restarts(other) != 0 {
		t.Fatalf("parked asleep, it let the next one go: %v", r.units.calls)
	}
	if err := r.sup.SetWanted(first, store.SessionDone); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if r.restarts(other) != 1 {
		t.Fatalf("ended, it no longer holds the others: %v", r.units.calls)
	}
}

// A session not due yet is not asked about again until it will be: the
// pass runs every ten seconds, and asking systemd each time is waste.
func TestAYoungSessionIsNotAskedAgainUntilDue(t *testing.T) {
	r := refreshRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.age(sess.ID, 10*time.Minute)
	r.reconcile()
	asked := r.units.sinceAsked
	r.reconcile()
	r.clock.jump(40 * time.Minute)
	r.reconcile()
	if r.units.sinceAsked != asked {
		t.Fatalf("asked %d more times before it was due", r.units.sinceAsked-asked)
	}
	r.clock.jump(15 * time.Minute)
	r.reconcile()
	if r.units.sinceAsked != asked+1 || r.restarts(sess.ID) != 1 {
		t.Fatalf("once due it is asked and restarted: asked %d, restarts %d", r.units.sinceAsked-asked, r.restarts(sess.ID))
	}
}

// What the registry says waits on a session holds its refresh too: an open
// card (a restart expires it) or a message not yet delivered. The same
// session with neither is restarted.
func TestARefreshWaitsForWhatTheRegistryHolds(t *testing.T) {
	for name, c := range map[string]struct {
		hold func(r *rig, id string)
		want int
	}{
		"nothing waits": {hold: func(*rig, string) {}, want: 1},
		"an open card": {hold: func(r *rig, id string) {
			r.st.UpsertSessionCard(store.SessionCard{ID: "c1", SessionID: id, Kind: "question", Body: "{}", State: store.CardOpen, CreatedAt: 1, UpdatedAt: 1})
		}},
		"a message waits": {hold: func(r *rig, id string) {
			r.st.AddInboxPromptAndWake(store.InboxPrompt{ID: "p1", SessionID: id, Text: "hi", CreatedAt: 1}, false, 1)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			r := refreshRig(t)
			sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
			r.reconcile()
			c.hold(r, sess.ID)
			if r.sup.refreshIfIdle(context.Background(), sess.ID, "test") != (c.want == 1) || r.restarts(sess.ID) != c.want {
				t.Fatalf("restarts %d, want %d: %v", r.restarts(sess.ID), c.want, r.units.calls)
			}
		})
	}
}

func TestSnapshotCold(t *testing.T) {
	now := time.UnixMilli(10_000_000)
	ended := func(ago time.Duration, ttl int64) string {
		return fmt.Sprintf(`{"result":{"endedAt":%d,"cacheTtlMs":%d}}`, now.Add(-ago).UnixMilli(), ttl)
	}
	cases := []struct {
		name, body string
		want       bool
	}{
		{"no turn finished yet: it has not answered what started it", `{}`, false},
		{"a turn that never ended", `{"result":{"endedAt":0}}`, false},
		{"warm: an hour's cache, ten minutes ago", ended(10*time.Minute, 3_600_000), false},
		{"cold: an hour's cache, an hour and a minute ago", ended(61*time.Minute, 3_600_000), true},
		{"cold on the default lifetime", ended(6*time.Minute, 0), true},
		{"a background task holds it", `{"info":{"tasks":[{}]},"result":{"endedAt":1}}`, false},
		{"an open question holds it", `{"questions":[{}],"result":{"endedAt":1}}`, false},
		{"unreadable", `{`, false},
	}
	for _, c := range cases {
		if got := snapshotQuiet(json.RawMessage(c.body), now, true); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
		// Finished waits for everything but the cache.
		finished := c.want || strings.HasPrefix(c.name, "warm")
		if got := snapshotQuiet(json.RawMessage(c.body), now, false); got != finished {
			t.Errorf("finished, %s: got %v", c.name, got)
		}
	}
}

// staleRig is a refresh rig whose loadout check says the sessions in stale
// are out of date.
func staleRig(t *testing.T) (*rig, map[string]bool) {
	r := refreshRig(t)
	stale := map[string]bool{}
	r.sup.cfg.StaleLoadouts = func() func(store.RunnerSession) bool {
		return func(sess store.RunnerSession) bool { return stale[sess.ID] }
	}
	return r, stale
}

// A session whose skills or plugins changed is started again at its next
// idle, cold moment, long before its age; once per change, so one that is
// still out of date after its restart waits for its age.
func TestASessionWithChangedSkillsIsStartedAgainEarly(t *testing.T) {
	r, stale := staleRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.age(sess.ID, 10*time.Minute)
	r.reconcile()
	if r.restarts(sess.ID) != 0 {
		t.Fatalf("restarted with nothing changed: %v", r.units.calls)
	}
	stale[sess.ID] = true
	r.reconcile()
	if r.restarts(sess.ID) != 1 {
		t.Fatalf("not restarted for changed skills: %v", r.units.calls)
	}
	row, _ := r.st.RunnerSession(sess.ID)
	if !strings.Contains(row.Note, "because its skills or plugins changed") {
		t.Fatalf("its note says why: %q", row.Note)
	}
	r.age(sess.ID, time.Minute)
	r.reconcile()
	r.reconcile()
	if r.restarts(sess.ID) != 1 {
		t.Fatalf("still out of date after its restart, restarted again: %v", r.units.calls)
	}
	stale[sess.ID] = false
	r.reconcile()
	stale[sess.ID] = true // a later change
	r.reconcile()
	if r.restarts(sess.ID) != 2 {
		t.Fatalf("a later change did not restart it: %v", r.units.calls)
	}
}

// Changed skills wait for the same quiet as an old session: a warm cache
// holds the restart.
func TestChangedSkillsWaitForAColdCache(t *testing.T) {
	r, stale := staleRig(t)
	r.clock.at = time.Now()
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.age(sess.ID, 10*time.Minute)
	r.run[sess.ID].snap = map[string]any{"result": map[string]any{"endedAt": time.Now().UnixMilli(), "cacheTtlMs": 3600000}}
	stale[sess.ID] = true
	r.reconcile()
	if r.restarts(sess.ID) != 0 {
		t.Fatalf("restarted on a warm cache: %v", r.units.calls)
	}
}

// A session that started on another Claude Code or SDK than a start now
// would run is started again at its next quiet moment, once per update.
// One whose spec says nothing of it (written by an older daemon) waits
// for its age.
func TestAnUpdatedClaudeOrSDKStartsASessionAgainEarly(t *testing.T) {
	r := refreshRig(t)
	now := RunsMark("/bin/claude", "/sc/serve.mjs", "") // what the rig's sessions start on
	r.sup.cfg.RunsNow = func() string { return now }
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.age(sess.ID, 10*time.Minute)
	r.reconcile()
	if r.restarts(sess.ID) != 0 {
		t.Fatalf("restarted on what it already runs: %v", r.units.calls)
	}
	now = RunsMark("/bin/claude", "/sdk/0.3.331/serve.mjs", "")
	r.reconcile()
	if r.restarts(sess.ID) != 1 {
		t.Fatalf("not restarted for a new SDK: %v", r.units.calls)
	}
	row, _ := r.st.RunnerSession(sess.ID)
	if !strings.Contains(row.Note, "Claude Code, the SDK or ccmux's session runner") {
		t.Fatalf("its note says why: %q", row.Note)
	}
	r.age(sess.ID, time.Minute)
	r.reconcile()
	r.reconcile()
	if r.restarts(sess.ID) != 1 {
		t.Fatalf("still on the old one after its restart, restarted again: %v", r.units.calls)
	}

	sp, _ := ReadSpec(r.paths.SpecFile(sess.ID))
	sp.Runs = ""
	if err := WriteSpec(r.paths.SpecFile(sess.ID), sp); err != nil {
		t.Fatal(err)
	}
	r.sup.refresh.markStale(sess.ID, false)
	r.reconcile()
	if r.restarts(sess.ID) != 1 {
		t.Fatalf("a spec that says nothing was taken as outdated: %v", r.units.calls)
	}
}

// The mark names the Claude Code a link points at, so moving the link
// (what Claude Code's updater does) changes it.
func TestRunsMarkFollowsTheClaudeLink(t *testing.T) {
	dir := t.TempDir()
	link := dir + "/claude"
	if err := os.Symlink(dir+"/v1", link); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(dir+"/v1", nil, 0o755)
	os.WriteFile(dir+"/v2", nil, 0o755)
	before := RunsMark(link, "/s.mjs", "")
	os.Remove(link)
	os.Symlink(dir+"/v2", link)
	if after := RunsMark(link, "/s.mjs", ""); after == before || !strings.Contains(after, "v2") {
		t.Fatalf("before %q after %q", before, after)
	}
}

// A deploy that changes the runner's own code (its files are rewritten in
// place, so the script's path stays) starts a session again early too.
func TestNewRunnerCodeStartsASessionAgainEarly(t *testing.T) {
	r := refreshRig(t)
	r.sup.cfg.RunnerCode = "code1"
	r.sup.cfg.RunsNow = func() string { return RunsMark("/bin/claude", "/sc/serve.mjs", "code1") }
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	r.age(sess.ID, 10*time.Minute)
	r.reconcile()
	if r.restarts(sess.ID) != 0 {
		t.Fatalf("restarted on the code it runs: %v", r.units.calls)
	}
	r.sup.cfg.RunsNow = func() string { return RunsMark("/bin/claude", "/sc/serve.mjs", "code2") }
	r.reconcile()
	if r.restarts(sess.ID) != 1 {
		t.Fatalf("not restarted for new runner code: %v", r.units.calls)
	}
}
