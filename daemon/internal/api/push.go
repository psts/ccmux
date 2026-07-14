package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// webPushSubscription mirrors the browser's PushSubscription.toJSON() so the POST
// body decodes directly. The stored Address is a canonical re-marshal of exactly
// these fields (dropping expirationTime and anything else), which is also the
// shape webpush-go's Subscription expects.
type webPushSubscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// pushVAPID hands the base64url VAPID public key to a lens, which passes it to
// pushManager.subscribe as applicationServerKey.
func (s *Server) pushVAPID(w http.ResponseWriter, _ *http.Request) {
	if s.sender == nil {
		writeError(w, http.StatusServiceUnavailable, "push not enabled")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": s.sender.PublicKey()})
}

// createSubscription stores (or replaces) a push subscription for the connecting
// dev. Identity is the verified tailnet login when available, else the
// self-declared user — the same key the notifier suppresses on. Re-subscribing
// with the same endpoint replaces the row (id is a hash of the endpoint).
func (s *Server) createSubscription(w http.ResponseWriter, r *http.Request) {
	if s.pushStore == nil {
		writeError(w, http.StatusServiceUnavailable, "push not enabled")
		return
	}
	var body webPushSubscription
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Endpoint == "" || body.Keys.P256dh == "" || body.Keys.Auth == "" {
		writeError(w, http.StatusBadRequest, "endpoint and keys required")
		return
	}
	// The daemon POSTs to this endpoint on every attention event, so a bogus one
	// is a server-side request forgery vector. Real push services are public
	// https; reject anything else (http, or a host pointing back inside the host/
	// tailnet).
	if err := validPushEndpoint(body.Endpoint); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	address, _ := json.Marshal(body) // canonical {endpoint, keys:{p256dh, auth}}
	sub := &model.PushSubscription{
		ID:        subscriptionID(body.Endpoint),
		Login:     s.loginKey(r),
		Transport: "webpush",
		Address:   string(address),
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := s.pushStore.SavePushSubscription(sub); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": sub.ID})
}

// deleteSubscription removes a subscription by its endpoint (the lens sends the
// endpoint it just unsubscribed). Idempotent: unknown endpoints still 204.
func (s *Server) deleteSubscription(w http.ResponseWriter, r *http.Request) {
	if s.pushStore == nil {
		writeError(w, http.StatusServiceUnavailable, "push not enabled")
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "endpoint required")
		return
	}
	if err := s.pushStore.DeletePushSubscription(subscriptionID(body.Endpoint)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// subscriptionView is the non-secret projection of a subscription returned to a
// settings UI: never the encryption keys, just enough to show "you're subscribed".
type subscriptionView struct {
	ID        string `json:"id"`
	Transport string `json:"transport"`
	CreatedAt int64  `json:"createdAt"`
}

// listSubscriptions returns the connecting dev's own subscriptions, so a settings
// page can reflect state across devices.
func (s *Server) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	if s.pushStore == nil {
		writeError(w, http.StatusServiceUnavailable, "push not enabled")
		return
	}
	login := s.loginKey(r)
	all, err := s.pushStore.ListPushSubscriptions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []subscriptionView{}
	for _, sub := range all {
		if sub.Login == login {
			out = append(out, subscriptionView{ID: sub.ID, Transport: sub.Transport, CreatedAt: sub.CreatedAt})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// loginKey resolves the identity a subscription is keyed on. It shares
// resolveIdentity with attach/presence, so a subscription and the same dev's
// attached lens land on the same key and suppression matches.
func (s *Server) loginKey(r *http.Request) string {
	return s.resolveIdentity(r).Login
}

// validPushEndpoint rejects endpoints that would turn the notifier into an SSRF
// primitive: it must be an https URL whose host is not a loopback/private/
// link-local literal. (A public hostname that later resolves to a private IP —
// DNS rebinding — is out of scope for this literal check.)
func validPushEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("unparseable endpoint")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("endpoint must be https")
	}
	host := u.Hostname()
	if host == "" || host == "localhost" {
		return fmt.Errorf("endpoint host not allowed")
	}
	if ip := net.ParseIP(host); ip != nil &&
		(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
		return fmt.Errorf("endpoint host not allowed")
	}
	return nil
}

// subscriptionID is a stable id for a push endpoint, so re-subscribing replaces
// rather than duplicates, and a delete-by-endpoint finds the row.
func subscriptionID(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:])
}
