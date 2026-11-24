package memtidy

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// record adds a repo's night to the log (log.jsonl in the state folder)
// and says it in the daemon's log.
func (t *Tidier) record(o outcome) {
	log.Printf("memory tidy: %s: %s (%d conversations read, %d unreadable, %d left for next night; +%d ~%d -%d) %s",
		o.Repo, o.Result, o.Conversations, len(o.Unreadable), o.Deferred, len(o.Added), len(o.Changed), len(o.Removed), o.Why)
	for _, u := range o.Unreadable {
		log.Printf("memory tidy: %s: unreadable, left out: %s", o.Repo, u)
	}
	b, err := json.Marshal(o)
	if err != nil {
		return
	}
	if err := os.MkdirAll(t.StateDir, 0o700); err != nil {
		log.Printf("memory tidy: %v", err)
		return
	}
	f, err := os.OpenFile(filepath.Join(t.StateDir, "log.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("memory tidy: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		log.Printf("memory tidy: %v", err)
	}
}
