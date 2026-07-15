// Bearer-token identity for the peers bus. Every mutating request must present
// a token derived from a persisted per-daemon secret: hosted panes get
// HMAC(secret, "pane:"+paneID) injected as CCMUX_PANE_TOKEN, and sessions
// without a pane share HMAC(secret, "pane-less") read from a 0600 daemon-info
// file. from_id must match the token's peer, which closes the old broker's
// unauthenticated-send hole (any local process — or a blind no-cors browser
// POST — could inject messages).
package peers

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// LoadOrCreateSecret reads the daemon's peers secret, minting (0600) a fresh
// 32-byte one on first run. Persisting it keeps pane tokens valid across
// daemon restarts — pane env is set once at pane creation.
func LoadOrCreateSecret(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if sec, err := hex.DecodeString(string(b)); err == nil && len(sec) == 32 {
			return sec, nil
		}
		return nil, fmt.Errorf("peers secret %s is corrupt — delete it to re-mint", path)
	}
	sec := make([]byte, 32)
	if _, err := rand.Read(sec); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(sec)), 0o600); err != nil {
		return nil, err
	}
	return sec, nil
}

func tokenFor(secret []byte, subject string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(subject))
	return hex.EncodeToString(mac.Sum(nil))
}

// TokenForPane is the bearer token a hosted pane's sessions authenticate with.
func TokenForPane(secret []byte, paneID string) string {
	return tokenFor(secret, "pane:"+paneID)
}

// PanelessToken is the shared bearer token for sessions with no ccmux pane
// (plain terminals), distributed via the daemon-info file.
func PanelessToken(secret []byte) string {
	return tokenFor(secret, "pane-less")
}

// DaemonInfo is the discovery record for pane-less sessions, which have no
// CCMUX_DAEMON_URL/CCMUX_PANE_TOKEN in their environment.
type DaemonInfo struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// WriteDaemonInfo writes the pane-less discovery file (0600).
func WriteDaemonInfo(path, url, token string) error {
	b, err := json.Marshal(DaemonInfo{URL: url, Token: token})
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// ReadDaemonInfo loads the discovery file (used by the thin client).
func ReadDaemonInfo(path string) (*DaemonInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var info DaemonInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	return &info, nil
}
