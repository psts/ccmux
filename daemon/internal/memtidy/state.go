package memtidy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// state is what the tidy-up keeps between nights: the local day it last
// finished a pass, and per repo the time up to which its conversations
// were read (unix ms), so each night reads only what is new.
type state struct {
	LastDay   string           `json:"lastDay,omitempty"`
	Bookmarks map[string]int64 `json:"bookmarks,omitempty"`
}

func (t *Tidier) stateFile() string { return filepath.Join(t.StateDir, "state.json") }

// loadState reads the state file; a missing one is a first night.
func (t *Tidier) loadState() (state, error) {
	st := state{Bookmarks: map[string]int64{}}
	b, err := os.ReadFile(t.stateFile())
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if st.Bookmarks == nil {
		st.Bookmarks = map[string]int64{}
	}
	return st, nil
}

func (t *Tidier) saveState(st state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(t.stateFile(), b)
}

// writeAtomic writes a file by rename, so a reader (or a crash) never sees
// half of it. The temp file is a new one of its own (O_EXCL, random name):
// a fixed name could be a link a session planted in a memory folder, and
// writing through it would land outside. The rename replaces a link at
// path itself, never what it points to.
func writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
