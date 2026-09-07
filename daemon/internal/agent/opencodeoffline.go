package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

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
		if SameDir(s.Directory, dir) {
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
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("opencode %v: %w: %s", args, err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("opencode %v: %w", args, err)
	}
	return out, nil
}

// SameDir says whether two folder paths are the same folder once symlinks
// are resolved: opencode records a session's directory as the real path,
// while a pane's cwd may be the name it was opened by (a migrated window
// folder kept as a symlink), and a session must not be lost to that.
func SameDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, erra := filepath.EvalSymlinks(a)
	rb, errb := filepath.EvalSymlinks(b)
	return erra == nil && errb == nil && ra == rb
}
