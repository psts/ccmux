package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

var (
	codeHashOnce sync.Once
	codeHash     string
)

// ClaudeServeCodeHash names the session runner's own code in this build:
// a hash of every file it ships (claudeServeShipped). A deploy rewrites
// those files in place, so the script's path alone does not change; the
// session refresh compares this (runner.RunsMark) to start a session that
// runs older code again at its next quiet moment.
func ClaudeServeCodeHash() string {
	codeHashOnce.Do(func() {
		h := sha256.New()
		for _, name := range claudeServeShipped {
			body, err := claudeServeFiles.ReadFile("claudeserve/" + name)
			if err != nil {
				body = []byte("missing")
			}
			h.Write([]byte(name + "\x00"))
			h.Write(body)
			h.Write([]byte{0})
		}
		codeHash = hex.EncodeToString(h.Sum(nil))[:16]
	})
	return codeHash
}
