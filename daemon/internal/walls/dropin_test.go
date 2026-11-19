package walls

import (
	"os"
	"path/filepath"
	"testing"
)

func TestADropInIsWrittenOnceAndRemovedWithItsFolder(t *testing.T) {
	dir := t.TempDir()
	unit := "ccmux-job-test@merge-task-1-abc.service"
	file := filepath.Join(dir, unit+".d", dropInName)
	for i, want := range []bool{true, false} {
		changed, err := SetDropIn(dir, unit, "[Service]\nPrivateTmp=yes\n")
		if err != nil || changed != want {
			t.Fatalf("write %d: changed=%v err=%v", i, changed, err)
		}
	}
	if b, _ := os.ReadFile(file); string(b) != "[Service]\nPrivateTmp=yes\n" {
		t.Fatalf("file: %q", b)
	}
	if changed, err := SetDropIn(dir, unit, "[Service]\nPrivateTmp=no\n"); err != nil || !changed {
		t.Fatalf("another text is a change: %v %v", changed, err)
	}
	if changed, err := SetDropIn(dir, unit, ""); err != nil || !changed {
		t.Fatalf("remove: %v %v", changed, err)
	}
	if _, err := os.Stat(filepath.Dir(file)); !os.IsNotExist(err) {
		t.Fatalf("the unit's drop-in folder goes with its last file: %v", err)
	}
	if changed, err := SetDropIn(dir, unit, ""); err != nil || changed {
		t.Fatalf("removing nothing changes nothing: %v %v", changed, err)
	}
}
