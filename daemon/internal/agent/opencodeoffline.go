package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"

	"ccmux.dev/ccmuxd/internal/harness"
)

// The offline reads: opencode's own CLI answers from its store without a
// server, which is how a chat view shows an asleep agent's history.

// OfflineSessions lists the sessions opened in dir (an instance folder),
// newest first, through `opencode session list --format json`.
func OfflineSessions(ctx context.Context, dir string) ([]OpencodeSession, error) {
	out, err := runOpencode(ctx, "session", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var all []OpencodeSession
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("opencode session list: %w", err)
	}
	mine := []OpencodeSession{}
	for _, s := range all {
		if s.Directory == dir {
			mine = append(mine, s)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].Updated > mine[j].Updated })
	return mine, nil
}

// OfflineTranscript is one session's conversation through `opencode export`.
func OfflineTranscript(ctx context.Context, sessionID string) ([]Turn, error) {
	out, err := runOpencode(ctx, "export", sessionID)
	if err != nil {
		return nil, err
	}
	return NormalizeExport(out)
}

func runOpencode(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := harness.LookPath("opencode")
	if err != nil {
		return nil, fmt.Errorf("opencode not installed: %w", err)
	}
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("opencode %v: %w", args, err)
	}
	return out, nil
}
