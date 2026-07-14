// Package push sends Web Push notifications (RFC 8291 message encryption + RFC
// 8292 VAPID) to browser/PWA subscriptions. It is transport-specific glue around
// github.com/SherClockHolmes/webpush-go — the daemon's notifier decides *whether*
// to send (attention + per-dev suppression); this package only knows *how*.
package push

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Keys is a persisted VAPID keypair (base64url strings, the form webpush-go and
// the browser's applicationServerKey both expect). The public key is handed to
// lenses; the private key signs the VAPID JWT and never leaves the host.
type Keys struct {
	Private string `json:"private"`
	Public  string `json:"public"`
}

// LoadOrCreateKeys returns the VAPID keypair persisted at path, generating and
// writing a fresh one (0600) on first run. The private key is a server secret,
// hence the restrictive mode and per-file directory creation.
func LoadOrCreateKeys(path string) (Keys, error) {
	if data, err := os.ReadFile(path); err == nil {
		var k Keys
		if err := json.Unmarshal(data, &k); err != nil {
			return Keys{}, fmt.Errorf("parse vapid keys %s: %w", path, err)
		}
		if k.Private == "" || k.Public == "" {
			return Keys{}, fmt.Errorf("vapid keys %s are incomplete", path)
		}
		return k, nil
	} else if !os.IsNotExist(err) {
		return Keys{}, fmt.Errorf("read vapid keys %s: %w", path, err)
	}

	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return Keys{}, fmt.Errorf("generate vapid keys: %w", err)
	}
	k := Keys{Private: priv, Public: pub}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Keys{}, fmt.Errorf("mkdir vapid dir: %w", err)
	}
	data, _ := json.Marshal(k)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return Keys{}, fmt.Errorf("write vapid keys %s: %w", path, err)
	}
	return k, nil
}
