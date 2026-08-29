// Teammate spawn (spawn_if_missing): preferred path is native — a live
// workspace in the sender's window whose repo basename matches the name gets
// an ephemeral pane running the frozen claude launch command. With no match
// (including pane-less requesters, whose group is a directory path with no
// window) it falls back to the old ccmux://spawn deep link with the classic
// parent-dir + name repo guess. Either way the triggering message is queued
// and delivered as a NORMAL peer message once the teammate registers — that
// seeds the permission relay's recent-senders set.
package peers

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// frozen contract with spawned teammates (mirrors SpawnRequest.swift): load
// the claude-peers channel, strip inherited identity overrides, end option
// parsing before the positional birth prompt.
const claudeLaunchPrefix = "env -u CLAUDE_PEERS_NAME -u CLAUDE_PEERS_PROJECT " +
	"claude --dangerously-load-development-channels server:claude-peers -- "

func claudeLaunchCommand(prompt string) string {
	return claudeLaunchPrefix + shellSingleQuote(prompt)
}

// shellSingleQuote wraps s for POSIX shells, replacing each embedded single
// quote with the close-escape-reopen idiom (port of SpawnRequest.shellSingleQuote).
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// birthPrompt is the teammate's seed prompt. It no longer tells the teammate
// to poll check_messages for its request: delivery is push, and the transcripts
// show spawned teammates polling empty five times while the request arrived as
// a channel message — the advice manufactured exactly the confusion it meant
// to prevent.
func birthPrompt(name, requesterName string) string {
	return fmt.Sprintf("You were just started as a claude-peers teammate %q to collaborate with peer %q. ", name, requesterName) +
		"On startup: call set_summary to say what you're doing. The request that started you is delivered " +
		`automatically as a <channel source="claude-peers"> message — do not poll for it. ` +
		`If it opens with "[claude-peers delegation task tsk_...]", follow its instructions and report progress via update_task; ` +
		fmt.Sprintf("otherwise complete it and report back to %q via send_message. ", requesterName) +
		fmt.Sprintf("If nothing arrives within a minute, call check_messages once as a fallback, then run list_peers and check in with %q directly.", requesterName)
}

func spawnKey(group, name string) string { return group + "\x00" + name }

// trySpawnLocked starts (or joins an in-flight start of) a missing teammate
// and queues the message. Slow work — tmux window creation, the `open` exec —
// runs outside the lock in a goroutine; registration or the timeout resolves
// the pending entry either way.
func (s *Service) trySpawnLocked(sender *Peer, group, name, text, toRepo string) SendResp {
	key := spawnKey(group, name)
	if pending := s.spawns[key]; pending != nil {
		pending.requests = append(pending.requests, queuedRequest{fromID: sender.ID, text: text})
		return SendResp{OK: true, Spawning: true}
	}

	prompt := birthPrompt(name, sender.Name)
	wsID, repoPath, native := s.mgr.LiveWorkspaceForRepo(group, name)
	if !native {
		// Classic repo guess: the requester's parent dir + name. For pane-less
		// requesters the group IS that parent dir, and this formula still lands
		// there because it derives from the requester's own path, not the group.
		repoPath = toRepo
		if repoPath == "" {
			base := sender.GitRoot
			if base == "" {
				base = sender.CWD
			}
			repoPath = filepath.Join(filepath.Dir(base), name)
		}
		if st, err := os.Stat(repoPath); err != nil || !st.IsDir() {
			return SendResp{Error: fmt.Sprintf("Cannot spawn %q: repo path %s does not exist", name, repoPath)}
		}
	}

	pending := &pendingSpawn{
		name: name, group: group, repo: repoPath,
		requests: []queuedRequest{{fromID: sender.ID, text: text}},
	}
	pending.timer = time.AfterFunc(s.SpawnTimeout, func() { s.spawnTimedOut(key) })
	s.spawns[key] = pending

	go func() {
		var err error
		if native {
			err = s.mgr.SpawnEphemeralPane(wsID, repoPath, claudeLaunchCommand(prompt), "claude-peers")
		} else {
			err = s.openSpawnURL(repoPath, prompt, sender.ID)
		}
		if err != nil {
			s.abortSpawn(key, fmt.Sprintf("Teammate %q could not be started: %v", name, err))
		}
	}()
	return SendResp{OK: true, Spawning: true}
}

// openSpawnURL fires the ccmux://spawn deep link, exactly like the old broker
// (the Mac app hosts the teammate in an ephemeral split pane).
func (s *Service) openSpawnURL(repo, prompt, requesterID string) error {
	if s.OpenCmd == "" {
		return fmt.Errorf("no live workspace hosts this repo and deep-link spawn is disabled")
	}
	q := url.Values{}
	q.Set("repo", repo)
	q.Set("prompt", prompt)
	q.Set("requester", requesterID)
	// Fire-and-forget: we never read the opener's output, but a Start with no
	// Wait leaves a defunct child for the daemon's life, one per spawn. Reap it
	// in the background rather than blocking the caller on a GUI handoff.
	cmd := exec.Command(s.OpenCmd, "ccmux://spawn?"+q.Encode())
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// fulfillPendingSpawnLocked delivers the queued request(s) the moment the
// spawned teammate registers — as normal peer messages, so the requester
// lands in the teammate's recent-senders set (permission relay reachability).
// Native teammates match by (group, name); a deep-link teammate registers
// pane-less in the dirname fallback group, so it matches by (name, repo) and
// gets pinned into the requester's group.
func (s *Service) fulfillPendingSpawnLocked(teammate *Peer) {
	key := spawnKey(s.groupOfLocked(teammate), teammate.Name)
	pending := s.spawns[key]
	if pending == nil {
		key, pending = s.matchSpawnByRepoLocked(teammate)
		if pending == nil {
			return
		}
		teammate.GroupOverride = pending.group
	}
	delete(s.spawns, key)
	pending.timer.Stop()
	for _, req := range pending.requests {
		requester := s.peers[req.fromID]
		if requester == nil {
			requester = &Peer{ID: req.fromID}
		}
		_ = s.deliverLocked(s.eventFromLocked(requester, teammate, pending.group, req.text))
	}
}

func (s *Service) matchSpawnByRepoLocked(teammate *Peer) (string, *pendingSpawn) {
	if teammate.PaneID != "" {
		return "", nil // daemon panes always resolve by exact group key
	}
	for key, pending := range s.spawns {
		if pending.name != teammate.Name {
			continue
		}
		if teammate.GitRoot == pending.repo || teammate.CWD == pending.repo ||
			strings.HasPrefix(teammate.CWD, pending.repo+string(filepath.Separator)) {
			return key, pending
		}
	}
	return "", nil
}

// spawnTimedOut tells every waiting requester the teammate never registered.
func (s *Service) spawnTimedOut(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.spawns[key]
	if pending == nil {
		return
	}
	delete(s.spawns, key)
	secs := int(s.SpawnTimeout / time.Second)
	text := fmt.Sprintf("Teammate %q could not be started — ccmux did not register it within %ds. "+
		"Check that ccmux is installed and able to host %s.", pending.name, secs, pending.repo)
	s.failTasksInLocked(pending, text)
	s.notifyRequestersLocked(pending, text)
}

// abortSpawn resolves a pending spawn early when starting it failed outright.
func (s *Service) abortSpawn(key, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.spawns[key]
	if pending == nil {
		return
	}
	delete(s.spawns, key)
	pending.timer.Stop()
	s.failTasksInLocked(pending, text)
	s.notifyRequestersLocked(pending, text)
}

func (s *Service) notifyRequestersLocked(pending *pendingSpawn, text string) {
	for _, req := range pending.requests {
		requester := s.peers[req.fromID]
		if requester == nil {
			continue
		}
		_ = s.deliverLocked(systemEvent(requester, pending.group, text))
	}
}
