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
	"encoding/json"
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
	action := announceUpgrade(repo, tag)
	if action == upgradeNothing {
		return nil
	}
	self, err := resolveSelf()
	if err != nil {
		return err
	}
	if action == upgradeRestart {
		// Nothing to download: the binaries are already the target. Only the
		// service is behind, and rewriting + restarting it is exactly what
		// install does. Same exec handoff as the tail of a real upgrade.
		return syscall.Exec(self, []string{self, "install"}, os.Environ())
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

// upgradeAction is what `ccmuxd upgrade` should do about the version it found.
type upgradeAction int

const (
	upgradeNothing upgradeAction = iota // on disk and running are both the target
	upgradeRestart                      // binaries are the target, the service is not running them
	upgradeFetch                        // fetch, swap, then hand off to install
)

// decideUpgrade is the pure rule (unit-tested). onDisk is the version of the
// binary running this command; running is what the live daemon reports, or ""
// when it could not be asked.
//
// The version comparison used to look ONLY at onDisk, and that is a state this
// tool can reach by itself: cmdUpgrade swaps the binaries and THEN execs
// install, so an interrupt in between leaves new binaries on disk and the old
// daemon still serving. Every later `ccmuxd upgrade` then reported "already up
// to date" and did nothing, while the running daemon stayed versions behind —
// on this fleet, for a whole day, until the missing routes were traced by hand.
// The binary on disk is not the thing being upgraded; the service is.
//
// An unreachable daemon means "" and is treated as needing the restart: the
// service being down IS the thing install fixes, and re-applying it is safe.
func decideUpgrade(onDisk, running, target string) upgradeAction {
	if onDisk != target {
		return upgradeFetch
	}
	if running == target {
		return upgradeNothing
	}
	return upgradeRestart
}

// announceUpgrade prints what is about to happen and reports what to do next.
func announceUpgrade(repo, tag string) upgradeAction {
	target := strings.TrimPrefix(tag, "v")
	running := runningVersion()
	action := decideUpgrade(version.Build, running, target)
	switch action {
	case upgradeNothing:
		fmt.Printf("already up to date (%s, and the service is running it)\n", version.Build)
	case upgradeRestart:
		fmt.Printf("binaries are already %s but the service is running %s — re-applying it\n",
			version.Build, orUnknown(running))
	case upgradeFetch:
		if version.Build == "dev" {
			fmt.Printf("upgrade: dev build → %s (release %s of %s)\n", target, tag, repo)
		} else {
			fmt.Printf("upgrade: %s → %s\n", version.Build, target)
		}
	}
	return action
}

func orUnknown(v string) string {
	if v == "" {
		return "an unknown version (it did not answer)"
	}
	return v
}

// runningVersion asks the live daemon what it is, over the loopback address the
// last install recorded. "" means "could not be asked" — no install on record,
// no daemon listening, or a reply this binary cannot read.
func runningVersion() string {
	saved, _ := loadPreviousInstall()
	if saved == nil || saved.Addr == "" {
		return ""
	}
	url := loopbackURL(saved.Addr)
	if url == "" {
		return ""
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url + "/v1/health")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var health struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return ""
	}
	return health.Version
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
