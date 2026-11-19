package walls

import (
	"bytes"
	"os"
	"path/filepath"
)

// dropInName is the walls file in a unit's drop-in folder.
const dropInName = "50-ccmux-walls.conf"

// DropInDir is the drop-in folder of one unit under dir (the user's
// systemd folder): systemd reads every .conf in it on top of the unit's
// template, for that one instance only.
func DropInDir(dir, unit string) string { return filepath.Join(dir, unit+".d") }

// SetDropIn makes the walls drop-in of one unit say text, or removes it
// for "" (a unit that runs open). It reports whether the file changed:
// systemd must then be reloaded before the unit starts, or it starts on
// what it read before.
func SetDropIn(dir, unit, text string) (changed bool, err error) {
	folder := DropInDir(dir, unit)
	path := filepath.Join(folder, dropInName)
	if text == "" {
		return removeDropIn(folder, path)
	}
	have, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err == nil && bytes.Equal(have, []byte(text)) {
		return false, nil
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// removeDropIn removes a unit's walls file, and its drop-in folder when
// that left it empty. A file that was not there is no change.
func removeDropIn(folder, path string) (bool, error) {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	_ = os.Remove(folder) // only when that left it empty
	return true, nil
}
