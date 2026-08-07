package api

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"

	"ccmux.dev/ccmuxd/internal/version"
)

// Remote-triggered self-upgrade: POST /v1/upgrade spawns a DETACHED
// `ccmuxd upgrade vX.Y.Z` child and answers 202 immediately. The child swaps
// the binaries and asks the init system to restart the service; the restart is
// driven by systemd/launchd, so it completes even as this process dies. Safe
// mid-job by construction: tmux survives the bounce (KillMode=process /
// launchd), lenses reconnect, and the peers bus is store-backed with cursor
// replay — a restart loses nothing. The hub proxies this per host
// (POST /v1/hosts/{host}/upgrade), which is what lets one lens upgrade the
// whole fleet.

var upgradeTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// upgrading guards against concurrent triggers (the 4h auto-check on several
// Macs could fire together).
var upgrading atomic.Bool

// selfUpgrade validates the request and hands off to the spawn seam.
func (s *Server) selfUpgrade(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version string `json:"version"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	tag := strings.TrimSpace(req.Version)
	if !upgradeTag.MatchString(tag) {
		writeError(w, http.StatusBadRequest, "version must be a release tag like v0.1.15")
		return
	}
	// A source build is a developer's working binary — refuse to clobber it
	// remotely, same rule the Mac updater applies locally.
	if b := version.Build; b == "dev" || strings.Contains(b, "-") {
		writeError(w, http.StatusConflict, fmt.Sprintf("this daemon runs a source build (%s) — upgrade it manually", b))
		return
	}
	if "v"+version.Build == tag {
		writeJSON(w, http.StatusOK, map[string]string{"status": "already " + version.Build})
		return
	}
	if !upgrading.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, "an upgrade is already running")
		return
	}
	if err := s.spawnUpgrade(tag); err != nil {
		upgrading.Store(false)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Deliberately NOT reset on success: the daemon restarts underneath us,
	// which is the only successful exit from this state.
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "upgrading", "target": tag})
}

// spawnUpgrade is the default spawn seam (tests replace it): run our own
// binary's upgrade verb in a NEW SESSION so the service bounce can't take the
// upgrader down with it, with its output kept for post-mortems. O_NOFOLLOW:
// the log lands in the shared /tmp on Linux (no PrivateTmp in the unit), so a
// pre-planted symlink must not redirect the write.
func realSpawnUpgrade(tag string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := fmt.Sprintf("%s/ccmuxd-upgrade-%d.log", os.TempDir(), os.Getuid())
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close() // the child holds its own inherited descriptor
	cmd := exec.Command(self, "upgrade", tag)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the child and clear the in-flight flag when it exits WITHOUT the
	// restart happening (download failure, checksum mismatch, already up to
	// date): otherwise one failed attempt would answer every later trigger
	// with "an upgrade is already running" until someone bounces the daemon.
	// On success the restart kills this process first, so the flag's fate is
	// irrelevant — a fresh daemon starts with it clear.
	go func() {
		err := cmd.Wait()
		upgrading.Store(false)
		log.Printf("upgrade child exited (err=%v) without a service restart — see %s", err, logPath)
	}()
	return nil
}
