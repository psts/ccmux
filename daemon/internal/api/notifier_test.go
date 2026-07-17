package api

import (
	"context"
	"net/http"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

type sentPush struct {
	address string
	topic   string
}

type fakeSender struct {
	sent   []sentPush
	status map[string]int // address -> status to return (default 201)
}

func (f *fakeSender) PublicKey() string { return "pub" }
func (f *fakeSender) Send(_ context.Context, address string, _ []byte, topic string) (int, error) {
	f.sent = append(f.sent, sentPush{address, topic})
	if st, ok := f.status[address]; ok {
		return st, nil
	}
	return http.StatusCreated, nil
}

type fakeStore struct {
	subs    []*model.PushSubscription
	deleted []string
}

func (f *fakeStore) SavePushSubscription(s *model.PushSubscription) error {
	f.subs = append(f.subs, s)
	return nil
}
func (f *fakeStore) ListPushSubscriptions() ([]*model.PushSubscription, error) { return f.subs, nil }
func (f *fakeStore) DeletePushSubscription(id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

type fakeFocus map[string]bool

func (f fakeFocus) ActiveOwners() map[string]bool { return f }

type fakeNamer struct{ ws *model.Workspace }

func (f fakeNamer) Workspace(string) *model.Workspace { return f.ws }

func newNotifier(sender pushSender, store pushStore, focus focusOracle) *notifier {
	return &notifier{sender: sender, subs: store, focus: focus, names: fakeNamer{ws: &model.Workspace{ID: "ws1", Name: "proj"}}}
}

func TestNotifier_NeedsInputPushesUnfocusedDevsOnly(t *testing.T) {
	sender := &fakeSender{}
	store := &fakeStore{subs: []*model.PushSubscription{
		{ID: "a", Login: "alice@example.com", Address: `{"endpoint":"e-alice"}`},
		{ID: "b", Login: "bob@example.com", Address: `{"endpoint":"e-bob"}`},
	}}
	// Alice has a focused lens somewhere (she's at a screen) → suppress her
	// push for ANY workspace's attention; Bob has none.
	focus := fakeFocus{"alice@example.com": true}

	newNotifier(sender, store, focus).onAttention(context.Background(), "ws1", model.AttentionNeedsInput)

	if len(sender.sent) != 1 {
		t.Fatalf("sent %d pushes, want 1 (only Bob)", len(sender.sent))
	}
	if sender.sent[0].address != `{"endpoint":"e-bob"}` {
		t.Errorf("pushed to %q, want Bob", sender.sent[0].address)
	}
}

func TestNotifier_IgnoresAmbientStates(t *testing.T) {
	for _, att := range []model.Attention{model.AttentionIdle, model.AttentionRunning} {
		sender := &fakeSender{}
		store := &fakeStore{subs: []*model.PushSubscription{{ID: "a", Login: "x", Address: `{"endpoint":"e"}`}}}
		newNotifier(sender, store, fakeFocus{}).onAttention(context.Background(), "ws1", att)
		if len(sender.sent) != 0 {
			t.Errorf("attention %q pushed %d times, want 0", att, len(sender.sent))
		}
	}
}

func TestNotifier_DonePushes(t *testing.T) {
	sender := &fakeSender{}
	store := &fakeStore{subs: []*model.PushSubscription{{ID: "a", Login: "x", Address: `{"endpoint":"e"}`}}}
	newNotifier(sender, store, fakeFocus{}).onAttention(context.Background(), "ws1", model.AttentionDone)
	if len(sender.sent) != 1 {
		t.Fatalf("done pushed %d times, want 1", len(sender.sent))
	}
}

func TestNotifier_PrunesDeadSubscriptions(t *testing.T) {
	sender := &fakeSender{status: map[string]int{
		`{"endpoint":"gone"}`: http.StatusGone,
		`{"endpoint":"nf"}`:   http.StatusNotFound,
	}}
	store := &fakeStore{subs: []*model.PushSubscription{
		{ID: "live", Login: "x", Address: `{"endpoint":"live"}`},
		{ID: "gone", Login: "x", Address: `{"endpoint":"gone"}`},
		{ID: "nf", Login: "x", Address: `{"endpoint":"nf"}`},
	}}
	newNotifier(sender, store, fakeFocus{}).onAttention(context.Background(), "ws1", model.AttentionNeedsInput)

	if len(sender.sent) != 3 {
		t.Fatalf("attempted %d sends, want 3", len(sender.sent))
	}
	got := map[string]bool{}
	for _, id := range store.deleted {
		got[id] = true
	}
	if !got["gone"] || !got["nf"] {
		t.Errorf("pruned %v, want both gone and nf", store.deleted)
	}
	if got["live"] {
		t.Error("pruned the live subscription (status 201)")
	}
}

func TestNotifier_PayloadCarriesTagAndDeepLink(t *testing.T) {
	n := newNotifier(&fakeSender{}, &fakeStore{}, fakeFocus{})
	p := n.payloadFor("ws1", model.AttentionNeedsInput)
	if p.Tag != "ws1" {
		t.Errorf("tag = %q, want ws1 (per-workspace replace)", p.Tag)
	}
	if p.URL != "/?ws=ws1" {
		t.Errorf("url = %q, want deep link /?ws=ws1", p.URL)
	}
	if p.Title != "proj" {
		t.Errorf("title = %q, want workspace name", p.Title)
	}
}
