package api

import (
	"context"
	"encoding/json"

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

// focusOracle reports which devs are actively watching a workspace (attached +
// focused), whose pushes the notifier suppresses. *presenceHub satisfies it.
type focusOracle interface {
	FocusedOwners(wsID string) map[string]bool
}

// workspaceNamer resolves a workspace id to metadata for the notification title.
// *manager.Manager satisfies it.
type workspaceNamer interface {
	Workspace(id string) *model.Workspace
}

// notifier turns pane-attention changes into Web Push notifications with per-dev
// suppression: a dev already watching the workspace (a lens attached AND focused)
// is skipped, because their lens is flashing the attention in-app. Subscriptions
// the push service reports as gone (404/410) are pruned.
type notifier struct {
	sender pushSender
	subs   pushStore
	focus  focusOracle
	names  workspaceNamer
}

// pushPayload is the JSON the service worker's `push` handler receives.
type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag"`   // = workspace id; a newer push replaces an older on the device
	URL   string `json:"url"`   // deep link the notificationclick opens
	State string `json:"state"` // attention state
}

// notifyState reports whether an attention transition is worth a push: the
// session either needs the user (needs_input) or just finished (done). running
// and idle are ambient and never notify.
func notifyState(att model.Attention) bool {
	return att == model.AttentionNeedsInput || att == model.AttentionDone
}

// onAttention sends a push for a pane's new attention state to every subscribed
// dev not currently watching the workspace, pruning dead subscriptions as it goes.
func (n *notifier) onAttention(ctx context.Context, wsID string, att model.Attention) {
	if !notifyState(att) {
		return
	}
	subs, err := n.subs.ListPushSubscriptions()
	if err != nil || len(subs) == 0 {
		return
	}
	suppressed := n.focus.FocusedOwners(wsID)
	body, _ := json.Marshal(n.payloadFor(wsID, att))
	topic := push.Topic(wsID)
	for _, sub := range subs {
		if suppressed[sub.Login] {
			continue
		}
		status, err := n.sender.Send(ctx, sub.Address, body, topic)
		if err != nil {
			continue
		}
		if push.Dead(status) {
			_ = n.subs.DeletePushSubscription(sub.ID)
		}
	}
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
