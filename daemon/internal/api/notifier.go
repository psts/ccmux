package api

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"ccmux.dev/ccmuxd/internal/hooktrace"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/push"
)

// pushSender sends one encrypted push and reports the push service's HTTP status
// (so the notifier can prune dead subscriptions). *push.Sender satisfies it; a
// fake satisfies it in tests.
type pushSender interface {
	Send(ctx context.Context, addressJSON string, payload []byte, topic string) (int, error)
	PublicKey() string
}

// pushStore is the slice of the registry the push endpoints and notifier use.
type pushStore interface {
	SavePushSubscription(*model.PushSubscription) error
	ListPushSubscriptions() ([]*model.PushSubscription, error)
	DeletePushSubscription(id string) error
}

// focusOracle reports which devs currently have a focused lens anywhere —
// they're at a screen, so the notifier suppresses their pushes entirely — and
// who is driving each workspace, for notification routing. *presenceHub
// satisfies it locally; *federatedFocus unions the members on a hub.
type focusOracle interface {
	ActiveOwners() map[string]bool
	DriverLogin(wsID string) (login string, atMillis int64, ok bool)
}

// workspaceNamer resolves a workspace id to metadata for the notification title.
// *manager.Manager satisfies it.
type workspaceNamer interface {
	Workspace(id string) *model.Workspace
}

// notifier turns pane-attention changes into Web Push notifications with per-dev
// suppression: a dev with any focused lens (they're at a screen — Mac app
// frontage or a visible web tab) is skipped for ALL workspaces, because the lens
// in front of them flashes or notifies locally. Subscriptions the push service
// reports as gone (404/410) are pruned.
type notifier struct {
	sender pushSender
	subs   pushStore
	focus  focusOracle
	names  workspaceNamer
	// audience is the notification-routing rule (Server.alertAudience): who a
	// workspace's attention belongs to. nil = unbounded (tests).
	audience func(wsID string) (map[string]bool, bool)
}

// pushPayload is the JSON the service worker's `push` handler receives.
type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag"`   // = workspace id; a newer push replaces an older on the device
	URL   string `json:"url"`   // deep link the notificationclick opens
	State string `json:"state"` // attention state
}

// notifyState reports whether an attention transition is worth a push.
//
// Only needs_input is. done is deliberately excluded, even though it still
// flashes the lens: done comes from the Stop hook alone, and Stop fires whenever
// Claude finishes *responding*, not when the work is finished. Measured over a
// day, 8 of 57 Stops were followed by another Stop in the same prompt — the turn
// carried on, sometimes spawning five more agents half a minute later, and
// nothing observable at the time predicted it.
//
// The signal that does mean "finished, and nothing more is coming" is Claude
// Code's own idle_prompt, which arrives 60s after the last Stop and resets on
// every new one. It maps to needs_input, so it still pushes here. That costs a
// minute of latency on a completion alert, which is invisible: respond inside
// the minute and no alert is wanted, and if you are away a minute does not
// matter. Flashing eagerly while pushing conservatively is the whole split.
func notifyState(att model.Attention) bool {
	return att == model.AttentionNeedsInput
}

// noPushReason explains a skipped push in the trace. done is worth its own
// sentence: it is the one state that flashes the lens but deliberately stays
// silent, and a reader who sees the flash and no alert should find out why here
// rather than assume something broke.
func noPushReason(att model.Attention) string {
	if att == model.AttentionDone {
		return "done flashes the lens; the alert waits for idle_prompt, which means the turn really ended"
	}
	return "state is ambient"
}

// onAttention sends a push for a pane's new attention state to every subscribed
// dev not currently watching the workspace, pruning dead subscriptions as it goes.
//
// Every branch is traced. Suppression compares two strings that are produced by
// different code paths for the same human — a lens's presence login and a
// subscription's stored login — so the trace records both sides of the comparison
// rather than only its result. A phone that buzzes while its owner is at a screen
// is almost always those two strings disagreeing, and no other log shows it.
func (n *notifier) onAttention(ctx context.Context, wsID string, att model.Attention) {
	if !notifyState(att) {
		n.trace(wsID, att, hooktrace.Line{Decision: "no-push", Detail: noPushReason(att)})
		return
	}
	subs, err := n.subs.ListPushSubscriptions()
	if err != nil {
		n.trace(wsID, att, hooktrace.Line{Decision: "no-push", Detail: "list subscriptions: " + err.Error()})
		return
	}
	if len(subs) == 0 {
		n.trace(wsID, att, hooktrace.Line{Decision: "no-push", Detail: "nobody is subscribed"})
		return
	}
	suppressed := n.focus.ActiveOwners()
	focused := focusedLogins(suppressed)
	audience, bounded := map[string]bool(nil), false
	if n.audience != nil {
		audience, bounded = n.audience(wsID)
	}
	body, _ := json.Marshal(n.payloadFor(wsID, att))
	topic := push.Topic(wsID)
	for _, sub := range subs {
		// Routing before suppression: not-your-repo beats at-a-screen, and the
		// trace should say WHICH rule kept the phone quiet.
		if bounded && !audience[sub.Login] {
			n.trace(wsID, att, hooktrace.Line{Decision: "routed-away", Login: sub.Login,
				Detail: "not this workspace's driver or window holder"})
			continue
		}
		if suppressed[sub.Login] {
			n.trace(wsID, att, hooktrace.Line{Decision: "suppressed", Login: sub.Login, Suppressed: sub.Login})
			continue
		}
		status, err := n.sender.Send(ctx, sub.Address, body, topic)
		if err != nil {
			n.trace(wsID, att, hooktrace.Line{Decision: "send-failed", Login: sub.Login, Detail: err.Error()})
			continue
		}
		if push.Dead(status) {
			_ = n.subs.DeletePushSubscription(sub.ID)
			n.trace(wsID, att, hooktrace.Line{Decision: "pruned", Login: sub.Login, Detail: "push service says gone"})
			continue
		}
		n.trace(wsID, att, hooktrace.Line{Decision: "sent", Login: sub.Login, Detail: "focused now: " + focused})
	}
}

// focusedLogins renders the suppression set for the trace. On a "sent" line this
// is the comparison that failed: if a login here looks like the same person as
// the one that just got pushed, the two identity paths disagree.
func focusedLogins(active map[string]bool) string {
	if len(active) == 0 {
		return "(nobody)"
	}
	names := make([]string, 0, len(active))
	for login := range active {
		names = append(names, login)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// trace fills in the fields every push line shares. There is no trace id here:
// the notifier consumes the manager firehose, which carries a workspace and an
// attention state but no hook identity, so these lines correlate with the route
// line above them by timestamp and workspace.
func (n *notifier) trace(wsID string, att model.Attention, line hooktrace.Line) {
	line.Stage = hooktrace.StagePush
	line.WorkspaceID = wsID
	line.Attention = string(att)
	hooktrace.Write(line)
}

func (n *notifier) payloadFor(wsID string, att model.Attention) pushPayload {
	name := wsID
	if ws := n.names.Workspace(wsID); ws != nil {
		name = orDefault(ws.Name, orDefault(ws.RepoPath, wsID))
	}
	body := "needs your input"
	if att == model.AttentionDone {
		body = "finished a task"
	}
	return pushPayload{Title: name, Body: body, Tag: wsID, URL: "/?ws=" + wsID, State: string(att)}
}

// run consumes the manager firehose and pushes for attention changes. Each event
// is handled in its own goroutine so a slow or unreachable push service can't
// stall the feed for other workspaces.
func (n *notifier) run(ctx context.Context, ch <-chan manager.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Kind == "attention" {
				go n.onAttention(ctx, ev.WorkspaceID, ev.Attention)
			}
		}
	}
}
