package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
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
		sub, err := fs.Sub(claudeServeFiles, "claudeserve")
		if err != nil {
			panic("claudeserve: embedded files: " + err.Error()) // the embed itself is broken
		}
		codeHash = codeHashOf(sub, claudeServeShipped)
	})
	return codeHash
}

// codeHashOf hashes the named files of fsys, names and contents both. A
// file that cannot be read hashes as missing: it fails every start anyway
// (EnsureClaudeServe), and TestClaudeServeShipsEveryImportedModule holds
// the shipped list to the embed.
func codeHashOf(fsys fs.FS, names []string) string {
	h := sha256.New()
	for _, name := range names {
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			body = []byte("\x00missing")
		}
		h.Write([]byte(name + "\x00"))
		h.Write(body)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
