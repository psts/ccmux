package agent

import (
	"context"
	"fmt"
	"sort"

	"ccmux.dev/ccmuxd/internal/harness"
)

// HasChat says whether an agent on this harness gets the chat view: the
// daemon can talk to a running instance from outside its terminal (a port
// on the startup line, see ChatPort) and read its history while asleep.
// opencode serves that itself; claude does through the sidecar. Every lens
// applies the same rule (app.js updateHarnessBar, PaneContentView.swift).
func HasChat(harnessName string) bool {
	return harnessName == "opencode" || harnessName == harness.Builtin
}

// OfflineSessions lists an asleep agent's conversations opened in dir,
// newest first, from the harness's own store: opencode's CLI, or the
// claude sidecar under root. A harness with no chat has no history to
// read, said as an error rather than an empty list.
func OfflineSessions(ctx context.Context, harnessName, root, dir string) ([]OpencodeSession, error) {
	switch harnessName {
	case "opencode":
		return opencodeOfflineSessions(ctx, dir)
	case harness.Builtin:
		return ClaudeOfflineSessions(ctx, root, dir)
	}
	return nil, fmt.Errorf("no chat history for the %s harness", harnessName)
}

// OfflineTranscript is one of those conversations.
func OfflineTranscript(ctx context.Context, harnessName, root, dir, sessionID string) ([]Turn, error) {
	switch harnessName {
	case "opencode":
		return opencodeOfflineTranscript(ctx, sessionID)
	case harness.Builtin:
		return ClaudeOfflineTranscript(ctx, root, dir, sessionID)
	}
	return nil, fmt.Errorf("no chat history for the %s harness", harnessName)
}

func sortNewestFirst(s []OpencodeSession) {
	sort.SliceStable(s, func(i, j int) bool { return s[i].Updated > s[j].Updated })
}
