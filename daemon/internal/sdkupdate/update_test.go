package sdkupdate

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
)

// fakeStore keeps the update's record and versions in memory, and says
// what was done to it.
type fakeStore struct {
	pinned, current string
	last            agent.SDKUpdate
	installErr      error
	did             []string
	pruned          []string // the scripts PruneSDKs was told sessions run from
	kept            int
}

func (f *fakeStore) SDKVersions() agent.SDKVersions {
	return agent.SDKVersions{Pinned: f.pinned, Current: f.current}
}
func (f *fakeStore) LastSDKUpdate() agent.SDKUpdate { return f.last }
func (f *fakeStore) RecordSDKUpdate(u agent.SDKUpdate) error {
	f.last = u
	return nil
}
func (f *fakeStore) InstallSDK(_ context.Context, v string) (string, error) {
	f.did = append(f.did, "install "+v)
	return "/c/" + v + "/serve.mjs", f.installErr
}
func (f *fakeStore) PromoteSDK(v string) error {
	f.did = append(f.did, "promote "+v)
	f.current = v
	return nil
}
func (f *fakeStore) RemoveSDK(v string) error {
	f.did = append(f.did, "remove "+v)
	return nil
}
func (f *fakeStore) PruneSDKs(keep int, inUse []string) error {
	f.did = append(f.did, "prune")
	f.kept, f.pruned = keep, inUse
	return nil
}

func updater(st *fakeStore, latest string, trialErr map[string]error) (*Updater, *[]string) {
	tried := &[]string{}
	return &Updater{
		Store: st, Every: 24 * time.Hour, Claude: "/bin/claude",
		Latest: func() (string, error) { return latest, nil },
		Trial: func(_ context.Context, script, claude string) error {
			*tried = append(*tried, script+" "+claude)
			return trialErr[claude]
		},
		now: func() time.Time { return time.UnixMilli(100 * 3600_000) },
	}, tried
}

// A newer SDK is installed, tried on the host's Claude and on its own, and
// only then promoted.
func TestANewerSDKIsTriedOnBothClaudesThenPromoted(t *testing.T) {
	st := &fakeStore{pinned: "0.3.311"}
	u, tried := updater(st, "0.3.330", nil)
	rec := u.Once(context.Background())
	if !rec.OK || rec.Version != "0.3.330" || st.current != "0.3.330" {
		t.Fatalf("record %+v, current %q", rec, st.current)
	}
	if !slices.Equal(*tried, []string{"/c/0.3.330/serve.mjs /bin/claude", "/c/0.3.330/serve.mjs "}) {
		t.Fatalf("trials %v", *tried)
	}
	if !slices.Equal(st.did, []string{"install 0.3.330", "promote 0.3.330", "prune"}) {
		t.Fatalf("did %v", st.did)
	}
	if st.last.At == 0 {
		t.Fatal("the try is recorded with its time")
	}
}

// A version whose session fails is removed, recorded as a failed test,
// and sessions keep what they had.
func TestAFailedTrialKeepsTheOldSDK(t *testing.T) {
	st := &fakeStore{pinned: "0.3.311", current: "0.3.320"}
	u, _ := updater(st, "0.3.330", map[string]error{"": errors.New("no answer within 3m")})
	rec := u.Once(context.Background())
	if rec.OK || rec.Kind != "test" || st.current != "0.3.320" {
		t.Fatalf("record %+v, current %q", rec, st.current)
	}
	if !slices.Equal(st.did, []string{"install 0.3.330", "remove 0.3.330"}) {
		t.Fatalf("did %v", st.did)
	}
}

// Nothing newer than what sessions run (the pin, or a promoted version):
// nothing installed. An install or npm that fails is not a failed test.
func TestNothingNewerAndInstallFailures(t *testing.T) {
	st := &fakeStore{pinned: "0.3.311", current: "0.3.330"}
	u, tried := updater(st, "0.3.330", nil)
	if rec := u.Once(context.Background()); !rec.OK || len(st.did) != 0 || len(*tried) != 0 {
		t.Fatalf("nothing newer: %+v %v %v", rec, st.did, *tried)
	}
	st = &fakeStore{pinned: "0.3.311", installErr: errors.New("npm: ETIMEDOUT")}
	u, tried = updater(st, "0.3.330", nil)
	if rec := u.Once(context.Background()); rec.OK || rec.Kind != "install" || len(*tried) != 0 || st.current != "" {
		t.Fatalf("install failed: %+v %v", rec, *tried)
	}
	u.Latest = func() (string, error) { return "", errors.New("offline") }
	if rec := u.Once(context.Background()); rec.OK || rec.Kind != "install" {
		t.Fatalf("npm unreachable: %+v", rec)
	}
}

// With no host claude, the trial runs on the SDK's own only.
func TestNoHostClaudeTriesTheSDKsOwn(t *testing.T) {
	st := &fakeStore{pinned: "0.3.311"}
	u, tried := updater(st, "0.3.330", nil)
	u.Claude = ""
	u.Once(context.Background())
	if !slices.Equal(*tried, []string{"/c/0.3.330/serve.mjs "}) {
		t.Fatalf("trials %v", *tried)
	}
}

// Due once Every has passed since the last try, whatever it came to.
func TestDue(t *testing.T) {
	st := &fakeStore{}
	u, _ := updater(st, "", nil)
	if !u.due() {
		t.Fatal("never tried: due")
	}
	now := u.clock()
	st.last.At = now.Add(-23 * time.Hour).UnixMilli()
	if u.due() {
		t.Fatal("tried 23h ago: not due")
	}
	st.last.At = now.Add(-25 * time.Hour).UnixMilli()
	if !u.due() {
		t.Fatal("tried 25h ago: due")
	}
}

func TestAnswered(t *testing.T) {
	for body, want := range map[string]struct {
		done bool
		err  bool
	}{
		`{"busy":true,"messages":[{"info":{"role":"assistant"},"parts":[{"type":"text","text":"OK"}]}]}`: {},
		`{"messages":[{"info":{"role":"user"},"parts":[{"type":"text","text":"hi"}]}]}`:                  {},
		`{"messages":[{"info":{"role":"assistant"},"parts":[{"type":"text","text":"OK"}]}]}`:             {done: true},
		`{"messages":[{"info":{"role":"assistant","error":{"name":"x"}},"parts":[]}]}`:                   {done: true, err: true},
		`{"messages":[{"info":{"role":"assistant","error":null},"parts":[{"type":"text","text":" "}]}]}`: {},
	} {
		done, err := answered(json.RawMessage(body))
		if done != want.done || (err != nil) != want.err {
			t.Errorf("%s: done %v err %v", body, done, err)
		}
	}
}

// Pruning after a promotion is told which sidecars session specs name, so
// a folder a running session starts from again is never removed; a failed
// trial prunes nothing.
func TestPruningKeepsWhatSessionsRunFrom(t *testing.T) {
	st := &fakeStore{pinned: "0.3.311"}
	u, _ := updater(st, "0.3.330", nil)
	u.InUse = func() []string { return []string{"/c/0.3.300/serve.mjs"} }
	u.Once(context.Background())
	if st.kept != keepVersions || !slices.Equal(st.pruned, []string{"/c/0.3.300/serve.mjs"}) {
		t.Fatalf("prune got keep %d, in use %v", st.kept, st.pruned)
	}
	st = &fakeStore{pinned: "0.3.311"}
	u, _ = updater(st, "0.3.330", map[string]error{"": errors.New("broken")})
	u.InUse = func() []string { t.Fatal("a failed trial asked what to keep"); return nil }
	u.Once(context.Background())
}

// A try the daemon's stopping cut off is not recorded and removes nothing:
// the next start tries again, and Settings blames no SDK for a restart.
func TestATryCutOffByShutdownIsNotRecorded(t *testing.T) {
	st := &fakeStore{pinned: "0.3.311"}
	ctx, cancel := context.WithCancel(context.Background())
	u, _ := updater(st, "0.3.330", nil)
	u.Trial = func(c context.Context, _, _ string) error { cancel(); return c.Err() }
	u.Once(ctx)
	if st.last.At != 0 || !u.due() {
		t.Fatalf("recorded %+v", st.last)
	}
	if slices.Contains(st.did, "remove 0.3.330") || st.current != "" {
		t.Fatalf("did %v, current %q", st.did, st.current)
	}
	// A real failure with the daemon running is still recorded.
	u.Trial = func(context.Context, string, string) error { return errors.New("no answer") }
	if rec := u.Once(context.Background()); rec.Kind != "test" || st.last.At == 0 {
		t.Fatalf("a real failure: %+v, recorded %+v", rec, st.last)
	}
}
