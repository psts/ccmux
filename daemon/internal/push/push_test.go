package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLoadOrCreateKeys_GeneratesThenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "vapid.json")

	first, err := LoadOrCreateKeys(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if first.Private == "" || first.Public == "" {
		t.Fatal("generated keypair is incomplete")
	}

	// File must exist with owner-only permissions (it holds a server secret).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("vapid file perm = %o, want 600", perm)
	}

	// Second load returns the same persisted keys, not a fresh pair.
	second, err := LoadOrCreateKeys(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if second != first {
		t.Errorf("keys changed across loads: %+v vs %+v", first, second)
	}
}

func TestLoadOrCreateKeys_RejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vapid.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKeys(path); err == nil {
		t.Error("expected error on corrupt keys file, got nil")
	}
}

func TestNewSender_NormalizesSubject(t *testing.T) {
	// webpush-go prepends mailto: to non-https subjects; a pre-prefixed mailto:
	// would double up and Apple rejects it (BadJwtToken). NewSender strips it.
	for in, want := range map[string]string{
		"mailto:ops@example.com": "ops@example.com",
		"ops@example.com":        "ops@example.com",
		"https://example.com":    "https://example.com",
	} {
		if got := NewSender(Keys{}, in).subject; got != want {
			t.Errorf("NewSender subject %q → %q, want %q", in, got, want)
		}
	}
}

func TestTopic_URLSafeAndBounded(t *testing.T) {
	got := Topic("11111111-2222-3333-4444-555555555555")
	if len(got) > 32 {
		t.Errorf("topic %q longer than 32 chars", got)
	}
	if got != "11111111222233334444555555555555" {
		t.Errorf("topic = %q, want dashes stripped", got)
	}
}

func TestDead(t *testing.T) {
	for _, tc := range []struct {
		status int
		dead   bool
	}{{404, true}, {410, true}, {201, false}, {429, false}, {500, false}} {
		if Dead(tc.status) != tc.dead {
			t.Errorf("Dead(%d) = %v, want %v", tc.status, Dead(tc.status), tc.dead)
		}
	}
}

// TestSend_PostsToEndpointWithTTL drives a real Send against a stub push service,
// asserting the encrypted request reaches the subscription's endpoint carrying
// the TTL and Topic we set, and that the service's status is returned verbatim so
// the notifier can prune. (The RFC 8291 ciphertext itself is webpush-go's tested
// concern; here we verify our wiring of it.)
func TestSend_PostsToEndpointWithTTL(t *testing.T) {
	var gotTTL, gotTopic, gotAuth, gotEncoding string
	var gotBodyLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTTL = r.Header.Get("TTL")
		gotTopic = r.Header.Get("Topic")
		gotAuth = r.Header.Get("Authorization")
		gotEncoding = r.Header.Get("Content-Encoding")
		buf := make([]byte, r.ContentLength)
		n, _ := r.Body.Read(buf)
		gotBodyLen = n
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	keys, err := LoadOrCreateKeys(filepath.Join(t.TempDir(), "vapid.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewSender(keys, "mailto:test@ccmux.local")

	// A subscription with a valid P-256 public key (p256dh) and 16-byte auth so
	// the ECDH/HKDF encryption path runs; the endpoint points at our stub.
	p256dh, auth := genSubscriptionKeys(t)
	address, _ := json.Marshal(map[string]any{
		"endpoint": srv.URL + "/push/abc",
		"keys": map[string]string{
			"p256dh": p256dh,
			"auth":   auth,
		},
	})

	status, err := s.Send(context.Background(), string(address), []byte(`{"title":"hi"}`), Topic("ws-123"))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if status != http.StatusCreated {
		t.Errorf("status = %d, want 201", status)
	}
	if ttl, _ := strconv.Atoi(gotTTL); ttl != pushTTL {
		t.Errorf("TTL header = %q, want %d", gotTTL, pushTTL)
	}
	if gotTopic != Topic("ws-123") {
		t.Errorf("Topic header = %q, want %q", gotTopic, Topic("ws-123"))
	}
	if gotEncoding != "aes128gcm" {
		t.Errorf("Content-Encoding = %q, want aes128gcm", gotEncoding)
	}
	if gotAuth == "" {
		t.Error("missing VAPID Authorization header")
	}
	if gotBodyLen == 0 {
		t.Error("encrypted body was empty")
	}
}

func TestSend_RejectsBadAddress(t *testing.T) {
	s := NewSender(Keys{Private: "x", Public: "y"}, "mailto:test@ccmux.local")
	if _, err := s.Send(context.Background(), "{not json", nil, "t"); err == nil {
		t.Error("expected error on malformed address JSON")
	}
	if _, err := s.Send(context.Background(), `{"keys":{}}`, nil, "t"); err == nil {
		t.Error("expected error on address with no endpoint")
	}
}

// genSubscriptionKeys mints a real P-256 subscription keypair: p256dh is the
// uncompressed public point (the exact bytes a browser's PushSubscription
// exposes) and auth is a 16-byte secret, both base64url — decodable by the ECDH
// handshake webpush-go runs.
func genSubscriptionKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authBytes := make([]byte, 16)
	if _, err := rand.Read(authBytes); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(authBytes)
}
