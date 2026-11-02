package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/version"
)

// autoUpgrade keeps a release build on the latest release: every `every` it
// reads the latest tag (the same releases/latest redirect `ccmuxd upgrade`
// follows) and, when that is newer than this build, starts the same detached
// upgrade POST /v1/upgrade starts. So a /ship reaches the daemon a few minutes
// after its tag with nobody running anything. Safe mid-job for the reason the
// route is: tmux survives the restart and the lenses reconnect.
//
// Only a binary GoReleaser built for a tag checks (version.Release). A source
// build never does, even one made from a clean tag checkout, which
// build-daemon.sh stamps exactly like a release: it is someone's working
// binary. every <= 0 turns the check off (-auto-upgrade 0).
//
// A tick that finds a newer tag whose files are not uploaded yet, or whose
// upgrade fails, leaves this daemon on its version; the next tick tries again.
func autoUpgrade(ctx context.Context, every time.Duration, latest func() (string, error), start func(tag string) (int, string)) {
	if every <= 0 || !selfUpgrades(version.Release, version.Build) {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		was := failing
		failing = autoUpgradeOnce(version.Build, latest, start, failing)
		if was && !failing {
			log.Printf("auto-upgrade: the latest release is readable again")
		}
	}
}

// autoUpgradeOnce is one tick (unit-tested). It returns whether reading the
// latest release failed, so the loop logs a failure on the way INTO it only:
// it runs every few minutes, and a line per tick while offline would bury the
// one that explains it.
func autoUpgradeOnce(current string, latest func() (string, error), start func(tag string) (int, string), failing bool) bool {
	tag, err := latest()
	if err != nil {
		if !failing {
			log.Printf("auto-upgrade: could not read the latest release: %v", err)
		}
		return true
	}
	if !newerRelease(tag, current) {
		return false
	}
	code, msg := start(tag)
	log.Printf("auto-upgrade: %s -> %s: %d %s", current, tag, code, msg)
	return false
}

// latestReleaseTag is the tag the next `ccmuxd upgrade` would install: the
// latest release of CCMUX_REPO, else of psts/ccmux.
func latestReleaseTag() (string, error) {
	repo := os.Getenv("CCMUX_REPO")
	if repo == "" {
		repo = defaultRepo
	}
	return resolveReleaseTag(repo, "")
}

// selfUpgrades says whether a binary with these stamps keeps itself current:
// GoReleaser marked it a release, and its version is plain numbers.
func selfUpgrades(release, build string) bool {
	return release == "true" && releaseNums(build) != nil
}

// newerRelease says whether tag ("v0.1.88") is a release newer than current
// ("0.1.87"), comparing numbers so 0.1.10 beats 0.1.9 and never going back.
func newerRelease(tag, current string) bool {
	if !strings.HasPrefix(tag, "v") {
		return false
	}
	a, b := releaseNums(tag[1:]), releaseNums(current)
	if a == nil || b == nil {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// releaseNums parses "X.Y.Z" into its three numbers, nil for anything else.
func releaseNums(v string) []int {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		nums[i] = n
	}
	return nums
}
