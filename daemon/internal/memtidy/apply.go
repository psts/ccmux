package memtidy

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Claude Code loads MEMORY.md at every start up to these (read in its
// code, 2.1.331: 200 lines, 25,000 bytes); what is past them is not seen.
const (
	indexLines = 200
	indexBytes = 25_000
)

// errChanged is a memory folder a session wrote to while the tidy-up ran:
// nothing is applied, and the next night tries again.
var errChanged = errors.New("a session changed this memory while it was tidied")

// files is a memory folder's notes: every .md file under it by its path
// in the folder, with its content. Dot files and anything else (a lock, a
// tool's state) are not notes, and the tidy-up leaves them alone.
type files map[string][]byte

func readNotes(dir string) (files, error) {
	out := files{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == dir {
				return fs.SkipDir
			}
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != dir {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".md") {
			rel, _ := filepath.Rel(dir, p)
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = b
		}
		return nil
	})
	return out, err
}

func writeNotes(dir string, notes files) error {
	for rel, b := range notes {
		if err := writeAtomic(filepath.Join(dir, rel), b); err != nil {
			return err
		}
	}
	return nil
}

// change is what a tidy-up did to one memory folder.
type change struct {
	Added, Changed, Removed []string
}

func (c change) empty() bool { return len(c.Added)+len(c.Changed)+len(c.Removed) == 0 }

func diff(old, new files) change {
	var c change
	for rel, b := range new {
		if o, ok := old[rel]; !ok {
			c.Added = append(c.Added, rel)
		} else if !bytes.Equal(o, b) {
			c.Changed = append(c.Changed, rel)
		}
	}
	for rel := range old {
		if _, ok := new[rel]; !ok {
			c.Removed = append(c.Removed, rel)
		}
	}
	sort.Strings(c.Added)
	sort.Strings(c.Changed)
	sort.Strings(c.Removed)
	return c
}

// check refuses a tidied memory that would do harm: no index, an index
// past what Claude Code loads, half the notes gone at once, or a secret
// that was not there before.
func check(old, new files, c change, secrets []string) error {
	idx, ok := new["MEMORY.md"]
	if !ok || len(bytes.TrimSpace(idx)) == 0 {
		return errors.New("no MEMORY.md left")
	}
	if n := bytes.Count(idx, []byte("\n")); n > indexLines || len(idx) > indexBytes {
		return fmt.Errorf("MEMORY.md is %d lines, %d bytes: past what Claude Code loads", n, len(idx))
	}
	if len(old) >= 6 && 2*len(c.Removed) > len(old) {
		return fmt.Errorf("it would remove %d of %d notes at once", len(c.Removed), len(old))
	}
	for _, rel := range append(append([]string{}, c.Added...), c.Changed...) {
		if s := newSecret(old[rel], new[rel], secrets); s != "" {
			return fmt.Errorf("%s would get what looks like a secret (%s)", rel, s)
		}
	}
	return nil
}

// apply puts a checked memory in place of the live one: only when the live
// one is still what was read, after a backup, each file by rename, and
// never outside the live folder (openLive).
func apply(live string, read, new files, c change, backup string) error {
	root, err := openLive(live)
	if err != nil {
		return err
	}
	defer root.Close()
	now, err := readNotes(live)
	if err != nil {
		return err
	}
	if !same(now, read) {
		return errChanged
	}
	if err := writeNotes(backup, read); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	put := files{}
	for _, rel := range append(append([]string{}, c.Added...), c.Changed...) {
		put[rel] = new[rel]
	}
	return putNotes(root, put, c.Removed)
}

func same(a, b files) bool {
	if len(a) != len(b) {
		return false
	}
	for rel, x := range a {
		if y, ok := b[rel]; !ok || !bytes.Equal(x, y) {
			return false
		}
	}
	return true
}
