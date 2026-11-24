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
// own and a rename, and removes the ones gone.
func putNotes(root *os.Root, put files, removed []string) error {
	for rel, b := range put {
		if err := rootWrite(root, filepath.ToSlash(rel), b); err != nil {
			return err
		}
	}
	for _, rel := range removed {
		if err := root.Remove(filepath.ToSlash(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
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
