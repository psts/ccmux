package main

import (
	"context"
	"log"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
)

// pluginPruneEvery is how often the daemon clears Claude Code's plugin
// records of worktrees that are gone. The tidy-up that removes a worktree
// runs walled, where ~/.claude is read only, so the daemon does it.
const pluginPruneEvery = time.Hour

// startPluginPrune sweeps once now and then every pluginPruneEvery until
// ctx ends (agent.PruneInstalledPlugins says what goes).
func startPluginPrune(ctx context.Context, worktrees string) {
	go func() {
		t := time.NewTicker(pluginPruneEvery)
		defer t.Stop()
		for {
			prunePluginRecords(worktrees)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// prunePluginRecords is one sweep, logged when it removed something or
// failed.
func prunePluginRecords(worktrees string) {
	dir, err := agent.ClaudeConfigDir()
	if err != nil {
		log.Printf("plugins: cannot find Claude Code's folder: %v", err)
		return
	}
	n, err := agent.PruneInstalledPlugins(agent.InstalledPluginsPath(dir), worktrees)
	if err != nil {
		log.Printf("plugins: pruning records of gone worktrees: %v", err)
		return
	}
	if n > 0 {
		log.Printf("plugins: removed %d record(s) of worktrees that are gone", n)
	}
}
