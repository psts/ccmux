package llmserve

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/llmproxy"
	"ccmux.dev/ccmuxd/internal/meridian"
)

// fakeSet records every Reconcile.
type fakeSet struct {
	mu    sync.Mutex
	calls [][]meridian.Spec
}

func (f *fakeSet) Reconcile(s []meridian.Spec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeSet) Status() map[string]meridian.Status {
	return map[string]meridian.Status{"sub": {Running: true, PID: 42}}
}

func (f *fakeSet) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeAccounts struct {
	mu   sync.Mutex
	accs []llmproxy.Account
	err  error
}

func (f *fakeAccounts) Accounts() ([]llmproxy.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accs, f.err
}

var subAccount = llmproxy.Account{Name: "sub", Kind: llmproxy.KindMeridian, BaseURL: "http://127.0.0.1:39999", APIKey: "tok"}

// The sidecar set follows the meridian accounts, and only those.
func TestSidecarsFollowTheMeridianAccounts(t *testing.T) {
	set := &fakeSet{}
	other := llmproxy.Account{Name: "key", Kind: "anthropic", APIKey: "k"}
	newSidecars(set, &fakeAccounts{accs: []llmproxy.Account{other, subAccount}}).reconcile()
	want := []meridian.Spec{{Name: "sub", BaseURL: subAccount.BaseURL, Token: "tok"}}
	if len(set.calls) != 1 || !reflect.DeepEqual(set.calls[0], want) {
		t.Fatalf("reconciled %+v, want one call with %+v", set.calls, want)
	}
}

// A database that cannot be read for a moment is not "no accounts": the
// running sidecars are left alone, never stopped.
func TestUnreadableAccountsLeaveTheSidecarsRunning(t *testing.T) {
	set := &fakeSet{}
	newSidecars(set, &fakeAccounts{err: errors.New("database is locked")}).reconcile()
	if set.count() != 0 {
		t.Fatalf("reconciled %+v on a read error; want the set left as it was", set.calls)
	}
}

// keep looks at once, and again on a poke without waiting for its tick.
func TestKeepReconcilesAtOnceAndOnAPoke(t *testing.T) {
	set := &fakeSet{}
	sc := newSidecars(set, &fakeAccounts{accs: []llmproxy.Account{subAccount}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sc.keep(ctx, time.Hour) }()
	waitFor(t, func() bool { return set.count() == 1 })
	sc.nudge()
	waitFor(t, func() bool { return set.count() == 2 })
	cancel()
	<-done
}

// The daemon reads the state through the client, and its poke reaches keep.
func TestDaemonReadsAndPokesTheSidecarsOverTheWire(t *testing.T) {
	set := &fakeSet{}
	sc := newSidecars(set, &fakeAccounts{})
	srv := httptest.NewServer(Handler(llmproxy.New(nil), Config{DB: "db"}, sc))
	defer srv.Close()
	c := NewClient(srv.URL)
	st, err := c.Sidecars(context.Background())
	if err != nil || !st["sub"].Running || st["sub"].PID != 42 {
		t.Fatalf("Sidecars = %+v, %v; want sub running as pid 42", st, err)
	}
	if err := c.ReconcileSidecars(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sc.poke:
	default:
		t.Fatal("the reconcile request did not poke the loop")
	}
}

// A stop of the service ends ctx first and drains; the sidecars must still
// be up through that, and go only when stop is called after it.
func TestSidecarsOutliveTheStopSignalUntilStop(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("not installed") }
	sv := meridian.New(context.Background(), missing)
	ctx, cancel := context.WithCancel(context.Background())
	_, stop := startSidecars(ctx, sv, &fakeAccounts{accs: []llmproxy.Account{subAccount}})
	waitFor(t, func() bool { return len(sv.Status()) == 1 })
	cancel() // the stop signal: the drain runs now
	time.Sleep(50 * time.Millisecond)
	if len(sv.Status()) != 1 {
		t.Fatal("the sidecar went with the stop signal, before the drain")
	}
	stop()
	if n := len(sv.Status()); n != 0 {
		t.Fatalf("%d sidecars left after stop", n)
	}
}

// Serve also stops on a listener's error, with ctx still live: stop must
// not wait for a signal that is not coming.
func TestSidecarsStopWithoutTheStopSignal(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("not installed") }
	sv := meridian.New(context.Background(), missing)
	_, stop := startSidecars(context.Background(), sv, &fakeAccounts{accs: []llmproxy.Account{subAccount}})
	waitFor(t, func() bool { return len(sv.Status()) == 1 })
	done := make(chan struct{})
	go func() { defer close(done); stop() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop hung with ctx still live")
	}
	if n := len(sv.Status()); n != 0 {
		t.Fatalf("%d sidecars left after stop", n)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
