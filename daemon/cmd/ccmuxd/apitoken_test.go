package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheAPITokenIsNewAtEveryStartAndOnlyThisUserReadsIt(t *testing.T) {
	t.Setenv("CCMUXD_ROOT", t.TempDir())
	first, err := mintAPIToken("http://127.0.0.1:7911")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 48 {
		t.Fatalf("24 random bytes, as hex: %q", first)
	}
	for _, file := range []string{apiTokenFile(), lensURLFile()} {
		fi, err := os.Stat(file)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", file, fi, err)
		}
	}
	if fi, _ := os.Stat(privateDir()); fi.Mode().Perm() != 0o700 || filepath.Dir(apiTokenFile()) != privateDir() {
		t.Fatalf("the token is in a folder of its own, which the walls hide whole: %v", fi.Mode())
	}
	if got, _ := os.ReadFile(apiTokenFile()); strings.TrimSpace(string(got)) != first {
		t.Fatalf("the file holds the token: %q", got)
	}
	link, _ := os.ReadFile(lensURLFile())
	if strings.TrimSpace(string(link)) != "http://ccmux.localhost:7911/?key="+first {
		t.Fatalf("the link opens the lens under its own name, with the token: %q", link)
	}
	// A file left open by an earlier start is closed again.
	os.Chmod(apiTokenFile(), 0o644)
	second, err := mintAPIToken("http://127.0.0.1:7911")
	if err != nil || second == first {
		t.Fatalf("every start has its own token: %v", err)
	}
	if fi, _ := os.Stat(apiTokenFile()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("the mode is put right: %v", fi.Mode())
	}
}

func TestTheTokenCommandsPrintWhatTheDaemonWrote(t *testing.T) {
	t.Setenv("CCMUXD_ROOT", t.TempDir())
	if err := cmdAPIToken(nil); err == nil || !strings.Contains(err.Error(), "is the daemon running") {
		t.Fatalf("with no daemon the error says where to look: %v", err)
	}
	if _, err := mintAPIToken("http://127.0.0.1:7911"); err != nil {
		t.Fatal(err)
	}
	if err := cmdAPIToken(nil); err != nil {
		t.Fatal(err)
	}
	if err := cmdLensURL(nil); err != nil {
		t.Fatal(err)
	}
}
