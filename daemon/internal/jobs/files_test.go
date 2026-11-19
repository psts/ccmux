package jobs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAResultIsReadOnlyInTheFormatThisBinaryReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j1", "result.json")
	if _, err := ReadResult(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no result yet is os.ErrNotExist: %v", err)
	}
	if err := WriteResult(path, Result{ID: "j1", Kind: "k", Output: []byte(`{"ok":true}`), EndedAt: 5}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadResult(path)
	var out struct{ OK bool }
	if err != nil || got.Version != Version || got.ID != "j1" || json.Unmarshal(got.Output, &out) != nil || !out.OK {
		t.Fatalf("a result written is read back, with the format it is in: %+v %v", got, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("a job's files are its owner's alone: %v", fi.Mode())
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("nothing of the write is left beside it: %v", err)
	}
	for _, other := range []string{`{"version":2,"id":"j1","output":{"ok":true}}`, `{"version":0,"id":"j1"}`, `{"id":"j1","output":{"ok":true}}`} {
		os.WriteFile(path, []byte(other), 0o600)
		var ver VersionError
		got, err := ReadResult(path)
		if !errors.As(err, &ver) || ver.Want != Version || got.ID != "" || got.Output != nil {
			t.Fatalf("a newer or older format is refused, and nothing of it is read: %+v %v", got, err)
		}
		if !strings.Contains(err.Error(), "not read at all") {
			t.Fatalf("and the refusal says so: %v", err)
		}
	}
	os.WriteFile(path, []byte(`{"version":1,"id":`), 0o600)
	if _, err := ReadResult(path); !errors.Is(err, ErrGarbled) {
		t.Fatalf("a file that is no result at all is garbled: %v", err)
	}
}

func TestASpecIsReadOnlyInTheFormatThisBinaryReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j1", "spec.json")
	if err := WriteSpec(path, Spec{ID: "j1", Kind: "k", Input: []byte(`{"n":1}`), Env: []string{"A=1"}, MaxSeconds: 9, Locks: "/l"}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSpec(path)
	if err != nil || got.Kind != "k" || got.MaxSeconds != 9 || got.Env[0] != "A=1" {
		t.Fatalf("a spec written is read back: %+v %v", got, err)
	}
	os.WriteFile(path, []byte(`{"version":7,"id":"j1","kind":"k"}`), 0o600)
	var ver VersionError
	if _, err := ReadSpec(path); !errors.As(err, &ver) || ver.Have != 7 {
		t.Fatalf("a spec in another format is refused: %v", err)
	}
	os.WriteFile(path, []byte(`{"version":1}`), 0o600)
	if _, err := ReadSpec(path); err == nil {
		t.Fatal("a spec that names no job is refused")
	}
}
