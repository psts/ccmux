package memtidy

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
)

// openLive opens a repo's live memory folder as an os.Root, so every write
// the tidy-up makes there stays inside it: a link a session planted in the
// folder that points out of it is refused, not followed. The folder itself
// and the project folder above it must be real folders, not links, and the
// root must be the folder that was checked.
func openLive(live string) (*os.Root, error) {
	if err := os.Mkdir(live, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	var info os.FileInfo
	for _, p := range []string{filepath.Dir(live), live} {
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("%s is a link or a file, not a folder: nothing is written there", p)
		}
		info = fi
	}
	root, err := os.OpenRoot(live)
	if err != nil {
		return nil, err
	}
	if fi, err := root.Stat("."); err != nil || !os.SameFile(fi, info) {
		root.Close()
		return nil, fmt.Errorf("%s changed while it was opened: nothing is written there", live)
	}
	return root, nil
}

// putNotes writes notes into the live root, each by a temp file of its
// own and a rename, and removes the ones gone. It returns what it wrote,
// so a write that fails partway can say what landed. A note under a linked
// folder is refused before anything is written; MEMORY.md goes last, so an
// index never names a note that is not there yet.
func putNotes(root *os.Root, put files, removed []string) ([]string, error) {
	names := make([]string, 0, len(put))
	for rel := range put {
		if err := underLink(root, filepath.ToSlash(rel)); err != nil {
			return nil, err
		}
		names = append(names, rel)
	}
	sort.Slice(names, func(i, j int) bool {
		return names[j] == "MEMORY.md" || (names[i] != "MEMORY.md" && names[i] < names[j])
	})
	var wrote []string
	for _, rel := range names {
		if err := rootWrite(root, filepath.ToSlash(rel), put[rel]); err != nil {
			return wrote, err
		}
		wrote = append(wrote, rel)
	}
	for _, rel := range removed {
		if err := root.Remove(filepath.ToSlash(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return wrote, err
		}
		wrote = append(wrote, rel+" (removed)")
	}
	return wrote, nil
}

// underLink refuses a note whose folder, or any folder above it inside the
// root, is a link.
func underLink(root *os.Root, name string) error {
	for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
		fi, err := root.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is under %s, a link: nothing is written", name, dir)
		}
	}
	return nil
}

func rootWrite(root *os.Root, name string, b []byte) error {
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	var r [8]byte
	rand.Read(r[:])
	tmp := path.Join(path.Dir(name), "."+path.Base(name)+"."+hex.EncodeToString(r[:])+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, name)
	}
	if err != nil {
		root.Remove(tmp)
	}
	return err
}
