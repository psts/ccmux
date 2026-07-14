package push

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// pushTTL is how long the push service should retain an undelivered message
// (seconds). ~1h matches an attention signal's usefulness: after that the
// session state has almost certainly moved on, so a stale "needs input" is noise.
const pushTTL = 3600

// Sender delivers an encrypted payload to a single Web Push subscription. It is
// safe for concurrent use (webpush.SendNotification builds a fresh request each
// call and the default http.Client is concurrent-safe).
type Sender struct {
	keys    Keys
	subject string             // VAPID "sub": a mailto:/https: identifying this server
	client  webpush.HTTPClient // nil → webpush-go's default *http.Client
}

// NewSender builds a Sender. subject must be a valid mailto: or https: URL (the
// VAPID JWT subject the push service attributes the send to).
func NewSender(keys Keys, subject string) *Sender {
	return &Sender{keys: keys, subject: subject}
}

// PublicKey returns the base64url VAPID public key a lens passes to
// pushManager.subscribe as applicationServerKey.
func (s *Sender) PublicKey() string { return s.keys.Public }

// Send encrypts payload and POSTs it to the subscription described by addressJSON
// (the browser's PushSubscription.toJSON(): {endpoint, keys:{p256dh, auth}}).
// topic collapses undelivered messages for the same workspace at the push
// service. It returns the push service's HTTP status so the caller can prune dead
// subscriptions (see Dead).
func (s *Sender) Send(ctx context.Context, addressJSON string, payload []byte, topic string) (int, error) {
	var sub webpush.Subscription
	if err := json.Unmarshal([]byte(addressJSON), &sub); err != nil {
		return 0, fmt.Errorf("bad subscription address: %w", err)
	}
	if sub.Endpoint == "" {
		return 0, fmt.Errorf("subscription has no endpoint")
	}
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &sub, &webpush.Options{
		HTTPClient:      s.client,
		Subscriber:      s.subject,
		VAPIDPublicKey:  s.keys.Public,
		VAPIDPrivateKey: s.keys.Private,
		TTL:             pushTTL,
		Topic:           topic,
		Urgency:         webpush.UrgencyHigh,
	})
	if err != nil {
		return 0, err
	}
	// Drain and close so the connection can be reused; the body carries only an
	// error description we don't surface.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// Dead reports whether a push status means the subscription is permanently gone
// and should be pruned: 404 Not Found and 410 Gone are the RFC 8030 signals that
// the endpoint no longer exists.
func Dead(status int) bool {
	return status == http.StatusNotFound || status == http.StatusGone
}

// Topic derives an RFC 8030 Topic header value from a workspace id: the header
// must be ≤32 chars from the URL-safe base64 alphabet, so we strip the UUID's
// dashes (leaving 32 hex chars, a valid subset) to get a stable per-workspace
// collapse key.
func Topic(workspaceID string) string {
	t := strings.ReplaceAll(workspaceID, "-", "")
	if len(t) > 32 {
		t = t[:32]
	}
	return t
}
