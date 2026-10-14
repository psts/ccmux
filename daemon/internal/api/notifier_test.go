package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/push"
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

func (f fakeFocus) DriverLogin(string) (string, int64, bool) { return "", 0, false }

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

	newNotifier(sender, store, focus).onAttention(context.Background(), "ws1", model.AttentionNeedsInput, false)

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
		newNotifier(sender, store, fakeFocus{}).onAttention(context.Background(), "ws1", att, false)
		if len(sender.sent) != 0 {
			t.Errorf("attention %q pushed %d times, want 0", att, len(sender.sent))
		}
	}
}

// done flashes the lens but must never push. It comes from the Stop hook alone,
// and Stop fires when Claude finishes RESPONDING, not when the work is finished:
// measured over a day, 8 of 57 Stops were followed by another Stop in the same
// prompt, sometimes spawning five more agents half a minute later. The signal
// that really means "finished" is Claude Code's idle_prompt, which arrives 60s
// after the last Stop and re-sends the done still showing with remind set.
func TestNotifier_DoneDoesNotPush(t *testing.T) {
	sender := &fakeSender{}
	store := &fakeStore{subs: []*model.PushSubscription{{ID: "a", Login: "x", Address: `{"endpoint":"e"}`}}}
	newNotifier(sender, store, fakeFocus{}).onAttention(context.Background(), "ws1", model.AttentionDone, false)
	if len(sender.sent) != 0 {
		t.Fatalf("done pushed %d times, want 0 — Stop is not a finish", len(sender.sent))
	}
}

// The other half of the same rule: the reminder is what carries the
// completion alert, on the done the row still shows. An idle pane never
// claims, so a reminder cannot push for it.
func TestNotifier_RemindedDonePushes(t *testing.T) {
	sender := &fakeSender{}
	store := &fakeStore{subs: []*model.PushSubscription{{ID: "a", Login: "x", Address: `{"endpoint":"e"}`}}}
	n := newNotifier(sender, store, fakeFocus{})
	n.onAttention(context.Background(), "ws1", model.AttentionIdle, true)
	if len(sender.sent) != 0 {
		t.Fatalf("reminded idle pushed %d times, want 0", len(sender.sent))
	}
	n.onAttention(context.Background(), "ws1", model.AttentionDone, true)
	if len(sender.sent) != 1 {
		t.Fatalf("reminded done pushed %d times, want 1", len(sender.sent))
	}
}

// chanSender reports each push on a channel: run sends from its own goroutines,
// so fakeSender's slice would race.
type chanSender struct{ topics chan string }

func (c chanSender) PublicKey() string { return "pub" }
func (c chanSender) Send(_ context.Context, _ string, _ []byte, topic string) (int, error) {
	c.topics <- topic
	return http.StatusCreated, nil
}

// The firehose is the only way a reminder reaches a phone, so run must carry
// the event's remind flag into onAttention. A plain done rides along and must
// not push at all.
func TestNotifier_RunCarriesRemindFromTheFirehose(t *testing.T) {
	sender := chanSender{topics: make(chan string, 4)}
	store := &fakeStore{subs: []*model.PushSubscription{{ID: "a", Login: "x", Address: `{"endpoint":"e"}`}}}
	n := newNotifier(sender, store, fakeFocus{})
	ch := make(chan manager.Event, 2)
	ch <- manager.Event{Kind: "attention", WorkspaceID: "plain", Attention: model.AttentionDone}
	ch <- manager.Event{Kind: "attention", WorkspaceID: "reminded", Attention: model.AttentionDone, Remind: true}
	close(ch)
	n.run(context.Background(), ch)

	select {
	case topic := <-sender.topics:
		if topic != push.Topic("reminded") {
			t.Fatalf("pushed topic %q, want the reminded workspace's", topic)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a reminded done from the firehose never pushed")
	}
	// Each event runs in its own goroutine, so the plain done's push could land
	// after the reminded one. Wait for it before calling the run clean.
	select {
	case topic := <-sender.topics:
		t.Fatalf("a second push went out (topic %q); a plain done must not push", topic)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestNotifier_NeedsInputPushes(t *testing.T) {
	sender := &fakeSender{}
	store := &fakeStore{subs: []*model.PushSubscription{{ID: "a", Login: "x", Address: `{"endpoint":"e"}`}}}
	newNotifier(sender, store, fakeFocus{}).onAttention(context.Background(), "ws1", model.AttentionNeedsInput, false)
	if len(sender.sent) != 1 {
		t.Fatalf("needs_input pushed %d times, want 1", len(sender.sent))
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
	newNotifier(sender, store, fakeFocus{}).onAttention(context.Background(), "ws1", model.AttentionNeedsInput, false)

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

// The push half of audience routing: a bounded audience routes a phone away
// (the trace names the reason) while the member's push still sends — a routing
// mistake here is a phone that never buzzes, so this is the branch to pin.
func TestNotifier_BoundedAudienceRoutesAway(t *testing.T) {
	sender := &fakeSender{}
	store := &fakeStore{subs: []*model.PushSubscription{
		{ID: "a", Login: "patric@x.com", Address: `{"endpoint":"e-patric"}`},
		{ID: "b", Login: "dasha@x.com", Address: `{"endpoint":"e-dasha"}`},
	}}
	n := newNotifier(sender, store, fakeFocus{}) // nobody at a screen
	n.audience = func(string) (map[string]bool, bool) {
		return map[string]bool{"patric@x.com": true}, true
	}

	n.onAttention(context.Background(), "ws1", model.AttentionNeedsInput, false)

	if len(sender.sent) != 1 || sender.sent[0].address != `{"endpoint":"e-patric"}` {
		t.Fatalf("sent = %+v; want only the audience member's phone", sender.sent)
	}
}

// An unbounded audience (ungrouped repo, no routing data) keeps the old
// everyone-not-at-a-screen behavior — routing must never silence by default.
func TestNotifier_UnboundedAudienceSendsToAll(t *testing.T) {
	sender := &fakeSender{}
	store := &fakeStore{subs: []*model.PushSubscription{
		{ID: "a", Login: "patric@x.com", Address: `{"endpoint":"e-patric"}`},
		{ID: "b", Login: "dasha@x.com", Address: `{"endpoint":"e-dasha"}`},
	}}
	n := newNotifier(sender, store, fakeFocus{})
	n.audience = func(string) (map[string]bool, bool) { return nil, false }

	n.onAttention(context.Background(), "ws1", model.AttentionNeedsInput, false)

	if len(sender.sent) != 2 {
		t.Fatalf("sent = %+v; want both phones", sender.sent)
	}
}
