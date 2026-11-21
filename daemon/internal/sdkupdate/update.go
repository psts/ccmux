// Package sdkupdate keeps the Claude Agent SDK that sessions start on
// current. Once a day it asks npm for the newest SDK; when that is newer
// than what sessions start on, it installs it into a folder of its own
// (agent.InstallSDK), runs a real session on it (Trial), and only when
// that works moves sessions onto it (agent.PromoteSDK). A version that
// fails stays off, its reason in the record the Settings page shows, and
// sessions keep what they had. Sessions already running move at their
// next start, which the runner's refresh (runner/refresh.go) brings
// within a day.
package sdkupdate

import (
	"context"
	"fmt"
	"log"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
)

// Store is what the update keeps its versions and record in.
type Store interface {
	SDKVersions() agent.SDKVersions
	LastSDKUpdate() agent.SDKUpdate
	RecordSDKUpdate(agent.SDKUpdate) error
	InstallSDK(ctx context.Context, version string) (script string, err error)
	PromoteSDK(version string) error
	RemoveSDK(version string) error
	PruneSDKs(keep int, inUse []string) error
}

// Updater runs the daily update.
type Updater struct {
	Store Store
	// Every is how long after the last try the next one is due; 0 never.
	Every time.Duration
	// Latest asks npm for the newest SDK.
	Latest func() (string, error)
	// Trial runs a real session on the sidecar script and says whether a
	// turn finished. Called once per Claude it is given: the host's own and
	// the SDK's bundled one ("").
	Trial func(ctx context.Context, script, claude string) error
	// Claude is the host's claude, "" when there is none.
	Claude string
	// InUse is the sidecar scripts session specs name, which pruning keeps.
	InUse func() []string
	// Cleanup removes what an update cut off by a daemon restart left (a
	// trial's runner and folder, a half install); run once, at start.
	Cleanup func()
	now     func() time.Time
}

const (
	checkEvery   = 15 * time.Minute
	installLimit = 5 * time.Minute
	keepVersions = 3
)

// Run checks every quarter hour whether a try is due. The record of the
// last try is a file, so a daemon restarted many times a day still tries
// once a day.
func (u *Updater) Run(ctx context.Context) {
	if u.Every <= 0 {
		return
	}
	if u.Cleanup != nil {
		u.Cleanup()
	}
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	for {
		if u.due() {
			u.Once(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (u *Updater) clock() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

func (u *Updater) due() bool {
	last := u.Store.LastSDKUpdate()
	return last.At == 0 || u.clock().Sub(time.UnixMilli(last.At)) >= u.Every
}

// Once is one try, recorded whatever it came to, unless the daemon was
// stopping (every deploy stops it): a try cut off proves nothing about the
// SDK, so it is not recorded and the next start tries again.
func (u *Updater) Once(ctx context.Context) agent.SDKUpdate {
	rec := u.try(ctx)
	if ctx.Err() != nil {
		log.Printf("sdk update: cut off by the daemon stopping; tried again at its next start")
		return rec
	}
	rec.At = u.clock().UnixMilli()
	if err := u.Store.RecordSDKUpdate(rec); err != nil {
		log.Printf("sdk update: record not saved: %v", err)
	}
	if rec.OK {
		log.Printf("sdk update: sessions start on SDK %s", u.runs())
	} else {
		log.Printf("sdk update: %s failed (%s): %s", rec.Version, rec.Kind, rec.Error)
	}
	return rec
}

// runs is the version sessions start on: a promoted one, else the pin.
func (u *Updater) runs() string {
	v := u.Store.SDKVersions()
	if v.Current != "" && agent.NewerVersion(v.Current, v.Pinned) {
		return v.Current
	}
	return v.Pinned
}

func (u *Updater) try(ctx context.Context) agent.SDKUpdate {
	latest, err := u.Latest()
	if err != nil || latest == "" {
		return agent.SDKUpdate{Kind: "install", Error: fmt.Sprintf("npm did not say the newest SDK: %v", err)}
	}
	if !agent.NewerVersion(latest, u.runs()) {
		return agent.SDKUpdate{Version: latest, OK: true}
	}
	installCtx, cancel := context.WithTimeout(ctx, installLimit)
	defer cancel()
	script, err := u.Store.InstallSDK(installCtx, latest)
	if err != nil {
		return agent.SDKUpdate{Version: latest, Kind: "install", Error: err.Error()}
	}
	if err := u.trials(ctx, script); err != nil {
		if ctx.Err() != nil {
			return agent.SDKUpdate{Version: latest, Kind: "install", Error: "cut off: " + err.Error()} // not recorded (Once); the folder is replaced at the next install
		}
		if rmErr := u.Store.RemoveSDK(latest); rmErr != nil {
			log.Printf("sdk update: %s not removed: %v", latest, rmErr)
		}
		return agent.SDKUpdate{Version: latest, Kind: "test", Error: err.Error()}
	}
	if err := u.Store.PromoteSDK(latest); err != nil {
		return agent.SDKUpdate{Version: latest, Kind: "install", Error: "not saved as current: " + err.Error()}
	}
	u.prune()
	return agent.SDKUpdate{Version: latest, OK: true}
}

// trials runs a session on the host's Claude (what people's sessions use)
// and on the SDK's own (what role agents use).
func (u *Updater) trials(ctx context.Context, script string) error {
	claudes := []string{""}
	if u.Claude != "" {
		claudes = []string{u.Claude, ""}
	}
	for _, c := range claudes {
		if err := u.Trial(ctx, script, c); err != nil {
			which := "the SDK's own Claude Code"
			if c != "" {
				which = c
			}
			return fmt.Errorf("a session on %s: %w", which, err)
		}
	}
	return nil
}

func (u *Updater) prune() {
	var inUse []string
	if u.InUse != nil {
		inUse = u.InUse()
	}
	if err := u.Store.PruneSDKs(keepVersions, inUse); err != nil {
		log.Printf("sdk update: old versions not removed: %v", err)
	}
}
