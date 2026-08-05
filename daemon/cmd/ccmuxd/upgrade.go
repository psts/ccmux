// Self-update: `ccmuxd upgrade [vX.Y.Z]` fetches a release, verifies its
// checksum, swaps both binaries in place, and hands off to the NEW binary's
// `install` (update mode: saved answers, no prompts) to rewrite + restart the
// service. The curl|sh courier stays for bootstrap only — this is the same
// flow with the binary as the canonical courier.
//
// The swap writes beside the target and renames over it: overwriting a running
// binary's file directly fails on Linux (ETXTBSY), while rename onto the path
// is atomic and leaves the running process on the old inode.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"ccmux.dev/ccmuxd/internal/version"
)

const defaultRepo = "psts/ccmux"

var upgradeHTTP = &http.Client{Timeout: 5 * time.Minute}

func cmdUpgrade(args []string) error {
	repo, requested, err := parseUpgradeArgs(args)
	if err != nil {
		return err
	}
	tag, err := resolveReleaseTag(repo, requested)
	if err != nil {
		return err
	}
	if !announceUpgrade(repo, tag) {
		return nil // already up to date
	}
	self, err := resolveSelf()
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "ccmux-upgrade-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	if err := fetchAndVerifyRelease(repo, tag, tmp); err != nil {
		return err
	}
	if err := swapBinaries(tmp, filepath.Dir(self)); err != nil {
		return err
	}
	fmt.Println("upgrade: binaries swapped — handing off to the new version's installer")
	// Exec, not fork: the rest of the upgrade (service rewrite, restart, saved
	// config) belongs to the NEW binary, exactly like install.sh's final line.
	// Exec never returns on success, so the deferred cleanup would leak the
	// downloaded tarball on every upgrade — remove explicitly first.
	os.RemoveAll(tmp)
	return syscall.Exec(self, []string{self, "install"}, os.Environ())
}

func parseUpgradeArgs(args []string) (repo, requested string, err error) {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	r := fs.String("repo", "", "GitHub repo hosting the releases (default: "+defaultRepo+", or CCMUX_REPO)")
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}
	repo = *r
	if repo == "" {
		repo = os.Getenv("CCMUX_REPO")
	}
	if repo == "" {
		repo = defaultRepo
	}
	return repo, fs.Arg(0), nil // Arg(0): optional explicit tag; empty = latest
}

// announceUpgrade prints what is about to happen and reports whether an
// upgrade is needed at all.
func announceUpgrade(repo, tag string) bool {
	target := strings.TrimPrefix(tag, "v")
	switch version.Build {
	case target:
		// The version check reads the ON-DISK binary that is running this very
		// command, so it also fires when a previous upgrade swapped binaries and
		// then failed before the service was rewritten. Name the recovery.
		fmt.Printf("already up to date (%s) — if a previous upgrade was interrupted, run `ccmuxd install` to re-apply the service\n", version.Build)
		return false
	case "dev":
		fmt.Printf("upgrade: dev build → %s (release %s of %s)\n", target, tag, repo)
	default:
		fmt.Printf("upgrade: %s → %s\n", version.Build, target)
	}
	return true
}

// resolveReleaseTag turns "latest" (empty) into the concrete tag by following
// the releases/latest redirect — pinning every later download to ONE release,
// so a publish happening mid-upgrade can't serve mixed files.
func resolveReleaseTag(repo, requested string) (string, error) {
	if requested != "" {
		if !strings.HasPrefix(requested, "v") {
			requested = "v" + requested
		}
		return requested, nil
	}
	resp, err := upgradeHTTP.Get("https://github.com/" + repo + "/releases/latest")
	if err != nil {
		return "", fmt.Errorf("resolve latest release: %w", err)
	}
	defer resp.Body.Close()
	final := resp.Request.URL.Path // .../releases/tag/vX.Y.Z after redirect
	tag := final[strings.LastIndexByte(final, '/')+1:]
	if !isReleaseTag(tag) {
		return "", fmt.Errorf("could not resolve the latest release of %s (redirect landed on %s)", repo, final)
	}
	return tag, nil
}

// isReleaseTag matches install.sh's v[0-9]* guard: a bare "v" prefix would
// accept any redirect target, e.g. the releases index of a repo with no
// releases at all.
func isReleaseTag(tag string) bool {
	return len(tag) > 1 && tag[0] == 'v' && tag[1] >= '0' && tag[1] <= '9'
}

func releaseAsset() string { return "ccmux_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz" }

// fetchAndVerifyRelease downloads the platform tarball + checksums for tag,
// verifies, and extracts ccmuxd + ccmux-peers into dir.
func fetchAndVerifyRelease(repo, tag, dir string) error {
	base := "https://github.com/" + repo + "/releases/download/" + tag + "/"
	asset := releaseAsset()
	tarball := filepath.Join(dir, asset)
	fmt.Printf("upgrade: fetching %s (%s)\n", asset, tag)
	if err := downloadFile(base+asset, tarball); err != nil {
		return err
	}
	sums := filepath.Join(dir, "checksums.txt")
	if err := downloadFile(base+"checksums.txt", sums); err != nil {
		return err
	}
	sumData, err := os.ReadFile(sums)
	if err != nil {
		return err
	}
	want := checksumFor(string(sumData), asset)
	if want == "" {
		return fmt.Errorf("no checksum listed for %s in %s", asset, tag)
	}
	got, err := sha256File(tarball)
	if err != nil {
		return err
	}
	if want != got {
		return fmt.Errorf("checksum mismatch for %s (want %s, got %s)", asset, want, got)
	}
	fmt.Println("upgrade: checksum ok")
	return extractTarGz(tarball, dir, "ccmuxd", "ccmux-peers")
}

func downloadFile(url, dest string) error {
	resp, err := upgradeHTTP.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %s", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// checksumFor finds the sha256 for name in a goreleaser checksums.txt body
// ("<hex>  <filename>" lines).
func checksumFor(body, name string) string {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			return fields[0]
		}
	}
	return ""
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractTarGz pulls exactly the named top-level files out of the tarball into
// dir. Anything else in the archive is ignored — the allowlist is the guard
// against a hostile tarball writing paths of its choosing.
func extractTarGz(tarball, dir string, names ...string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[n] = true
	}
	if err := extractWanted(tar.NewReader(gz), dir, wanted); err != nil {
		return err
	}
	if len(wanted) > 0 {
		missing := make([]string, 0, len(wanted))
		for n := range wanted {
			missing = append(missing, n)
		}
		return fmt.Errorf("release archive is missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

// extractWanted walks the archive, writing each still-wanted regular file into
// dir (by BASE name — the allowlist doubles as the traversal guard) and
// removing it from wanted.
func extractWanted(tr *tar.Reader, dir string, wanted map[string]bool) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Base(hdr.Name)
		if hdr.Typeflag != tar.TypeReg || !wanted[name] {
			continue
		}
		if err := writeFileFrom(tr, filepath.Join(dir, name)); err != nil {
			return err
		}
		delete(wanted, name)
	}
}

func writeFileFrom(r io.Reader, dest string) error {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// swapBinaries installs each extracted binary over its counterpart in binDir
// via write-beside + rename: atomic, and legal while the old binary runs.
// ccmuxd goes LAST: a failure between the two renames then leaves the OLD
// ccmuxd on disk, so its version check still reports the old version and a
// retried `ccmuxd upgrade` actually retries instead of claiming up-to-date.
func swapBinaries(srcDir, binDir string) error {
	for _, b := range []string{"ccmux-peers", "ccmuxd"} {
		src := filepath.Join(srcDir, b)
		dest := filepath.Join(binDir, b)
		staged := dest + ".new"
		if err := copyFile(src, staged); err != nil {
			return fmt.Errorf("stage %s: %w", b, err)
		}
		if err := os.Rename(staged, dest); err != nil {
			os.Remove(staged)
			return fmt.Errorf("swap %s: %w", b, err)
		}
	}
	return nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFileFrom(in, dest)
}
