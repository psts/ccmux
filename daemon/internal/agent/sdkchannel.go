package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/harness"
)

// The SDK channel: a newer Claude Agent SDK than this daemon pins, taken
// from npm by the daily update (internal/sdkupdate) once a real session
// ran on it. Each version is a whole sidecar of its own, scripts and
// node_modules, under SDKChannelDir/<version>: a session runs from the
// folder its spec names, so a runner that started on one version is never
// touched by the next. The pinned install under ClaudeServeDir stays as
// it is (the offline reads and the probe use its fixed path), and serves
// whenever no newer version passed its test.
//
//	<root>/.ccmux/claude-sdk/<version>/   one sidecar per version
//	<root>/.ccmux/claude-sdk/current.json the version sessions start on
//	<root>/.ccmux/claude-sdk/status.json  the last update: when, which, how it went

// SDKChannelDir holds the versions under the agents root.
const SDKChannelDir = ".ccmux/claude-sdk"

// SDKUpdate is how the last daily update went. Kind is "" when it worked,
// "test" when the version installed but a session on it failed, and
// "install" when it could not be tried (npm or the network).
type SDKUpdate struct {
	At      int64  `json:"at"`
	Version string `json:"version"`
	OK      bool   `json:"ok"`
	Kind    string `json:"kind,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (s *Store) channelDir() string { return filepath.Join(s.Root, SDKChannelDir) }

// SDKCurrent is the version the update moved sessions onto, "" for none.
func (s *Store) SDKCurrent() string {
	var cur struct {
		Version string `json:"version"`
	}
	body, err := os.ReadFile(filepath.Join(s.channelDir(), "current.json"))
	if err != nil || json.Unmarshal(body, &cur) != nil {
		return ""
	}
	return cur.Version
}

// LastSDKUpdate is the last update's record; the zero value before any.
func (s *Store) LastSDKUpdate() SDKUpdate {
	var u SDKUpdate
	body, err := os.ReadFile(filepath.Join(s.channelDir(), "status.json"))
	if err == nil {
		_ = json.Unmarshal(body, &u)
	}
	return u
}

// RecordSDKUpdate saves how an update went.
func (s *Store) RecordSDKUpdate(u SDKUpdate) error {
	return s.writeChannelFile("status.json", u)
}

// PromoteSDK makes version the one sessions start on from now.
func (s *Store) PromoteSDK(version string) error {
	return s.writeChannelFile("current.json", map[string]any{"version": version, "at": time.Now().UnixMilli()})
}

func (s *Store) writeChannelFile(name string, v any) error {
	if err := os.MkdirAll(s.channelDir(), 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	path := filepath.Join(s.channelDir(), name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// channelSidecar is the sidecar of the promoted version, when it is newer
// than the pin and installed: its folder with this daemon's scripts
// written in. ok false sends the caller to the pinned install.
func (s *Store) channelSidecar() (script string, ok bool, err error) {
	cur := s.SDKCurrent()
	pinned, _ := pinnedSDKVersion()
	if cur == "" || !NewerVersion(cur, pinned) {
		return "", false, nil
	}
	dir := filepath.Join(s.channelDir(), cur)
	if installedSDKVersion(dir) != cur {
		return "", false, nil
	}
	if err := writeSidecarScripts(dir); err != nil {
		return "", false, err
	}
	return filepath.Join(dir, claudeServeFile), true, nil
}

// writeSidecarScripts writes this daemon's sidecar into dir, all but the
// manifest and lock, which name the folder's own SDK version.
func writeSidecarScripts(dir string) error {
	for _, name := range claudeServeShipped {
		if name == "package.json" || name == "package-lock.json" {
			continue
		}
		body, err := claudeServeFiles.ReadFile("claudeserve/" + name)
		if err != nil {
			return err
		}
		if _, err := writeIfChanged(filepath.Join(dir, name), body); err != nil {
			return err
		}
	}
	return nil
}

// InstallSDK puts version in a folder of its own, ready to test, and
// returns its sidecar script. It installs into a temporary name and
// renames it into place, so a half install is never a version, and it
// takes no lock session starts wait on.
func (s *Store) InstallSDK(ctx context.Context, version string) (string, error) {
	if _, ok := versionParts(version); !ok {
		return "", fmt.Errorf("not a version: %q", version) // it names a folder
	}
	node, err := harness.LookPath("node")
	if err != nil {
		return "", err
	}
	npm, err := harness.LookPath("npm")
	if err != nil {
		return "", err
	}
	tmp, err := s.stageSDK(version)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp) // gone once renamed; a failed one goes here
	cmd := exec.CommandContext(ctx, npm, "install", "--no-audit", "--no-fund", "--loglevel=error", "--save-exact", sdkPackage+"@"+version)
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("npm install %s@%s: %w: %s", sdkPackage, version, err, strings.TrimSpace(string(out)))
	}
	if got := installedSDKVersion(tmp); got != version {
		return "", fmt.Errorf("npm install left SDK %q, wanted %q", got, version)
	}
	dir := filepath.Join(s.channelDir(), version)
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, claudeServeFile), nil
}

// stageSDK makes the temporary folder an install goes into: the sidecar's
// scripts and a manifest that names version.
func (s *Store) stageSDK(version string) (string, error) {
	if err := os.MkdirAll(s.channelDir(), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(s.channelDir(), ".install-")
	if err != nil {
		return "", err
	}
	manifest := map[string]any{"name": "ccmux-claude-serve", "private": true, "type": "module", "dependencies": map[string]string{sdkPackage: version}}
	body, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "package.json"), body, 0o644); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	if err := writeSidecarScripts(tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return tmp, nil
}

// CleanSDKInstalls removes the temporary folders of installs a daemon
// restart cut off. Only at start, before any install: one in progress is
// one of these too.
func (s *Store) CleanSDKInstalls() {
	left, _ := filepath.Glob(filepath.Join(s.channelDir(), ".install-*"))
	for _, dir := range left {
		_ = os.RemoveAll(dir)
	}
}

// RemoveSDK deletes a version's folder: one whose test failed.
func (s *Store) RemoveSDK(version string) error {
	if version == "" || strings.ContainsAny(version, `/\`) || strings.HasPrefix(version, ".") {
		return fmt.Errorf("not a version: %q", version)
	}
	return os.RemoveAll(filepath.Join(s.channelDir(), version))
}

// PruneSDKs deletes the versions no session can be running from: neither
// the current one nor the newest keep others, nor one inUse names (a
// script path, as a session's spec holds it).
func (s *Store) PruneSDKs(keep int, inUse []string) error {
	entries, err := os.ReadDir(s.channelDir())
	if err != nil {
		return nil // no channel yet
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			versions = append(versions, e.Name())
		}
	}
	slices.SortFunc(versions, func(a, b string) int { return compareVersions(b, a) })
	cur := s.SDKCurrent()
	for i, v := range versions {
		dir := filepath.Join(s.channelDir(), v)
		if i < keep || v == cur || namesDir(inUse, dir) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}

func namesDir(scripts []string, dir string) bool {
	return slices.ContainsFunc(scripts, func(p string) bool { return filepath.Dir(p) == dir })
}

// NewerVersion says whether a is a later x.y.z than b; anything that is
// not one is never newer.
func NewerVersion(a, b string) bool { return compareVersions(a, b) > 0 }

func compareVersions(a, b string) int {
	pa, okA := versionParts(a)
	pb, okB := versionParts(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	return slices.Compare(pa, pb)
}

func versionParts(v string) ([]int, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil, false
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}
