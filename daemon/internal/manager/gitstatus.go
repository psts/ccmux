package manager

import (
	"encoding/json"
	"time"

	"ccmux.dev/ccmuxd/internal/gitstatus"
	"ccmux.dev/ccmuxd/internal/model"
)

// StartGitStatus begins the daemon-side git dashboard collector: every interval
// it recomputes git status for each LIVE workspace's repo and, when a
// workspace's status changed, stores it on the model (served in /v1/workspaces)
// and publishes a "workspace-git" firehose event so lenses refetch immediately.
//
// Polling, not fsnotify: portable to the future Linux host, immune to
// watch-descriptor exhaustion on big repos, and a warm `git status` costs
// single-digit milliseconds. Runs until the manager's ctx is cancelled. Called
// from main (not Start) so unit tests asserting exact firehose sequences don't
// see surprise events.
//
// A repo nobody works in (no terminal and no runner session there) is read
// too, but only once an hour (idleGitEvery): the thread page lists every
// repo with its branch, and such a repo's state rarely moves. Once a session
// starts in it, it is read every interval again.
func (m *Manager) StartGitStatus(interval time.Duration) {
	go func() {
		c := newGitCollector(m, idleGitEvery, time.Now)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-t.C:
				c.collectOnce()
			}
		}
	}()
}

// idleGitEvery is how often a repo nobody works in has its git state read.
const idleGitEvery = time.Hour

// gitCollector holds per-workspace collection state across ticks.
type gitCollector struct {
	m         *Manager
	last      map[string]string    // wsID → last published status (marshaled) for change detection
	defaults  map[string]string    // wsID → cached default branch; key absent = not yet resolved
	idleRead  map[string]time.Time // wsID → when an idle repo was last read; absent = read it now
	idleEvery time.Duration
	now       func() time.Time
}

func newGitCollector(m *Manager, idleEvery time.Duration, now func() time.Time) *gitCollector {
	return &gitCollector{m: m, last: map[string]string{}, defaults: map[string]string{}, idleRead: map[string]time.Time{}, idleEvery: idleEvery, now: now}
}

func (c *gitCollector) collectOnce() {
	seen := map[string]bool{}
	for _, ws := range c.m.List() {
		seen[ws.ID] = true
		if c.due(ws) {
			c.collectWorkspace(ws.ID, ws.RepoPath)
		}
	}
	// Drop state for removed workspaces.
	for id := range c.last {
		if !seen[id] {
			delete(c.last, id)
			delete(c.defaults, id)
		}
	}
	for id := range c.idleRead {
		if !seen[id] {
			delete(c.idleRead, id)
		}
	}
}

// due says whether a workspace is read this pass. One worked in (a live
// terminal session, or a runner session, which changes the tree with no
// terminal open) is read every pass; an idle one when it was never read or
// idleEvery has passed. A repo that goes idle is read once more at once.
func (c *gitCollector) due(ws *model.Workspace) bool {
	if ws.Status == model.StatusLive || c.m.hasRunnerPanes(ws.ID) {
		delete(c.idleRead, ws.ID)
		return true
	}
	now := c.now()
	if at, ok := c.idleRead[ws.ID]; ok && now.Sub(at) < c.idleEvery {
		return false
	}
	c.idleRead[ws.ID] = now
	return true
}

func (c *gitCollector) collectWorkspace(wsID, repoPath string) {
	st, err := gitstatus.Full(c.m.ctx, repoPath, c.defaults[wsID])
	if err != nil {
		return // transient (git missing/timeout) — keep the previous status
	}
	// Resolve the comparison base once per repo (mirrors the app's
	// GitStatusMonitor), then recompute so the "vs default ↑↓" row populates.
	if _, resolved := c.defaults[wsID]; st.IsGitRepo && !resolved {
		c.defaults[wsID] = gitstatus.DetectDefaultBranch(c.m.ctx, repoPath)
		if st, err = gitstatus.Full(c.m.ctx, repoPath, c.defaults[wsID]); err != nil {
			return
		}
	}
	blob, err := json.Marshal(st)
	if err != nil || c.last[wsID] == string(blob) {
		return
	}
	c.last[wsID] = string(blob)
	c.m.mu.Lock()
	if e := c.m.byID[wsID]; e != nil {
		e.ws.Git = st
	}
	c.m.mu.Unlock()
	c.m.events.publish(Event{Kind: "workspace-git", WorkspaceID: wsID})
}
