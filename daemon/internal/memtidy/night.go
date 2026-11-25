package memtidy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Night is what a lens is shown of a repo's last night.
type Night struct {
	At            string  `json:"at"`
	Result        string  `json:"result"`
	CostUSD       float64 `json:"costUSD"`
	Conversations int     `json:"conversations"`
	Todos         int     `json:"todos,omitempty"`
}

// LastNight is the newest line of the log (log.jsonl in stateDir) for any
// of the repo's names; nil when the log has none. The whole file is read:
// it gains a line per repo per night, and a line can be long.
func LastNight(stateDir string, names ...string) (*Night, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, "log.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range names {
		want[filepath.Clean(n)] = true
	}
	var last *Night
	for _, line := range bytes.Split(b, []byte("\n")) {
		var o outcome
		if json.Unmarshal(line, &o) != nil || !want[filepath.Clean(o.Repo)] {
			continue
		}
		last = &Night{At: o.At, Result: o.Result, CostUSD: o.CostUSD, Conversations: o.Conversations, Todos: o.Todos}
	}
	return last, nil
}
