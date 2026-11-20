package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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
	for _, file := range []string{apiTokenFile(), apiURLFile()} {
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
	if base, _ := os.ReadFile(apiURLFile()); strings.TrimSpace(string(base)) != "http://127.0.0.1:7911" {
		t.Fatalf("the daemon's address is kept for the commands: %q", base)
	}
	for _, file := range []string{apiTokenFile(), apiURLFile()} {
		if b, _ := os.ReadFile(file); strings.Contains(string(b), "key=") {
			t.Fatalf("no link with a key in it is ever kept in a file: %s", file)
		}
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

// The lens link is asked of the running daemon, with the token, and
// carries the one-use key it answers with: never the token.
func TestTheLensLinkCarriesAOneUseKeyNotTheToken(t *testing.T) {
	t.Setenv("CCMUXD_ROOT", t.TempDir())
	if _, err := lensLink(http.DefaultClient); err == nil || !strings.Contains(err.Error(), "is the daemon running") {
		t.Fatalf("with no daemon the error says where to look: %v", err)
	}
	var token string
	asked := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/lens-key" || r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"no API token"}`))
			return
		}
		w.Write([]byte(`{"key":"one-use-key","seconds":120}`))
	}))
	defer daemon.Close()
	var err error
	if token, err = mintAPIToken(daemon.URL); err != nil {
		t.Fatal(err)
	}
	link, err := lensLink(daemon.Client())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(daemon.URL)
	if link != "http://ccmux.localhost:"+u.Port()+"/?key=one-use-key" || asked != 1 {
		t.Fatalf("the link opens the lens under its own name, with the daemon's key: %q (asked %d)", link, asked)
	}
	if strings.Contains(link, token) {
		t.Fatal("the token is never in a link")
	}
	// A daemon that refuses (the token file is another start's) is said as
	// that, not printed as a link that will not work.
	os.WriteFile(apiTokenFile(), []byte("stale\n"), 0o600)
	if link, err := lensLink(daemon.Client()); err == nil || !strings.Contains(err.Error(), "gave no key") || link != "" {
		t.Fatalf("a refusal: %q %v", link, err)
	}
}

func TestTheTokenCommandReadsWhatTheDaemonWrote(t *testing.T) {
	t.Setenv("CCMUXD_ROOT", t.TempDir())
	if err := cmdAPIToken(nil); err == nil || !strings.Contains(err.Error(), "is the daemon running") {
		t.Fatalf("with no daemon the error says where to look: %v", err)
	}
	token, err := mintAPIToken("http://127.0.0.1:7911")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := readPrivate(apiTokenFile()); err != nil || got != token {
		t.Fatalf("the command reads the token, trimmed: %q %v", got, err)
	}
}
