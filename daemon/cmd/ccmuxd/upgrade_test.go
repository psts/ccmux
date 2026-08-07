package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/version"
)

func TestChecksumFor(t *testing.T) {
	body := "abc123  ccmux_linux_amd64.tar.gz\ndef456  ccmux_darwin_arm64.tar.gz\n"
	if got := checksumFor(body, "ccmux_darwin_arm64.tar.gz"); got != "def456" {
		t.Fatalf("checksum = %q", got)
	}
	if got := checksumFor(body, "ccmux_linux_arm64.tar.gz"); got != "" {
		t.Fatalf("missing asset yielded %q, want empty", got)
	}
}

func TestReleaseAsset_MatchesGoreleaserNaming(t *testing.T) {
	want := "ccmux_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	if got := releaseAsset(); got != want {
		t.Fatalf("asset = %q, want %q", got, want)
	}
}

// makeTarball builds a gzipped tar with the given name→content entries.
func makeTarball(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractTarGz_AllowlistAndTraversalGuard(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "rel.tar.gz")
	makeTarball(t, tarball, map[string]string{
		"ccmuxd":              "daemon-bytes",
		"ccmux-peers":         "peers-bytes",
		"README.md":           "ignored",
		"../escape/ccmuxd":    "hostile", // base name matches, path must not escape
		"nested/dir/ccmuxd":   "also-flattened-to-base",
		"not-wanted-anywhere": "ignored",
	})
	out := t.TempDir()
	if err := extractTarGz(tarball, out, "ccmuxd", "ccmux-peers"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 2 {
		t.Fatalf("extracted %d files, want exactly the 2 allowlisted", len(entries))
	}
	for _, name := range []string{"ccmuxd", "ccmux-peers"} {
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("%s not extracted: %v", name, err)
		}
		if len(data) == 0 {
			t.Fatalf("%s extracted empty", name)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(out), "escape", "ccmuxd")); err == nil {
		t.Fatal("path traversal escaped the extraction dir")
	}
}

func TestExtractTarGz_MissingBinaryFails(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "rel.tar.gz")
	makeTarball(t, tarball, map[string]string{"ccmuxd": "only-one"})
	err := extractTarGz(tarball, t.TempDir(), "ccmuxd", "ccmux-peers")
	if err == nil || !strings.Contains(err.Error(), "ccmux-peers") {
		t.Fatalf("missing binary not reported: %v", err)
	}
}

func TestSwapBinaries_RenamesOverExisting(t *testing.T) {
	src := t.TempDir()
	bin := t.TempDir()
	for _, b := range []string{"ccmuxd", "ccmux-peers"} {
		if err := os.WriteFile(filepath.Join(src, b), []byte("new-"+b), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, b), []byte("old-"+b), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := swapBinaries(src, bin); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"ccmuxd", "ccmux-peers"} {
		data, _ := os.ReadFile(filepath.Join(bin, b))
		if string(data) != "new-"+b {
			t.Fatalf("%s = %q after swap", b, data)
		}
		info, _ := os.Stat(filepath.Join(bin, b))
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode = %v, want 0755", b, info.Mode().Perm())
		}
		if _, err := os.Stat(filepath.Join(bin, b+".new")); err == nil {
			t.Fatalf("staging file %s.new left behind", b)
		}
	}
}

// decideUpgrade is the one rule coupled to goreleaser's version format
// (version.Build = tag without the v, per .goreleaser.yaml ldflags). It is what
// makes `upgrade` a no-op on the current version, what lets a retried upgrade
// proceed after a partial swap left the old ccmuxd on disk, and — the case that
// cost a day — what notices that the SERVICE is still on the old binary.
func TestDecideUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		onDisk, running, target string
		want                    upgradeAction
	}{
		{"current everywhere", "0.1.3", "0.1.3", "0.1.3", upgradeNothing},
		{"newer release out", "0.1.3", "0.1.3", "0.1.4", upgradeFetch},
		{"dev build", "dev", "dev", "0.1.3", upgradeFetch},
		{"partial swap: old binary on disk", "0.1.3", "0.1.4", "0.1.4", upgradeFetch},
		// The regression: binaries swapped, the exec handoff to install never
		// happened, so the service kept serving the old one. Reported "already
		// up to date" forever, because only the on-disk version was consulted.
		{"swapped but never restarted", "0.1.4", "0.1.3", "0.1.4", upgradeRestart},
		{"daemon not answering", "0.1.4", "", "0.1.4", upgradeRestart},
		{"source build serving itself", "0.1.4-dirty", "0.1.4-dirty", "0.1.4", upgradeFetch},
	} {
		if got := decideUpgrade(tc.onDisk, tc.running, tc.target); got != tc.want {
			t.Errorf("%s: decideUpgrade(%q, %q, %q) = %v, want %v",
				tc.name, tc.onDisk, tc.running, tc.target, got, tc.want)
		}
	}
}

// version.Build still has to be the value the rule reads, so the goreleaser
// coupling stays pinned even though the decision itself is now pure.
func TestAnnounceUpgrade_ReadsBuildVersion(t *testing.T) {
	orig := version.Build
	defer func() { version.Build = orig }()
	version.Build = "0.1.3"
	if got := announceUpgrade("r", "v0.1.4"); got != upgradeFetch {
		t.Fatalf("a newer release gave %v, want upgradeFetch", got)
	}
}

func TestIsReleaseTag(t *testing.T) {
	for tag, want := range map[string]bool{
		"v0.1.3": true, "v10.2.0": true,
		"v": false, "latest": false, "vnext": false, "": false, "releases": false,
	} {
		if got := isReleaseTag(tag); got != want {
			t.Errorf("isReleaseTag(%q) = %v, want %v", tag, got, want)
		}
	}
}

func TestSha256File(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// sha256("hello")
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	got, err := sha256File(p)
	if err != nil || got != want {
		t.Fatalf("sha256 = %q, %v", got, err)
	}
}
