package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/memtidy"
	"ccmux.dev/ccmuxd/internal/store"
)

// memoryTidyLimit is how long one repo's tidy-up may take.
const memoryTidyLimit = 30 * time.Minute

// startMemoryTidy runs the nightly tidy-up of each repo's Claude Code
// memory (internal/memtidy). A scratch daemon runs none unless
// -memory-tidy-hour is given: a test must not rewrite the person's memory.
func startMemoryTidy(ctx context.Context, st *store.SQLite, hour int, model string) {
	if scratchRoot() != "" && !flagGiven("memory-tidy-hour") {
		hour = -1
	}
	if hour < 0 || hour > 23 {
		log.Printf("memory tidy: off")
		return
	}
	claude, err := harness.LookPath("claude")
	if err != nil {
		log.Printf("memory tidy: off, no claude on this host: %v", err)
		return
	}
	home, _ := os.UserHomeDir()
	t := &memtidy.Tidier{
		Sessions: st.RunnerSessions,
		Projects: filepath.Join(home, ".claude", "projects"),
		StateDir: filepath.Join(filepath.Dir(runnerStateDir()), "memory-tidy"),
		Hour:     hour,
		Claude:   memtidy.Headless(claude, model, append(runnerEnv(), "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1"), memoryTidyLimit),
		DreamOn: func() bool {
			return memtidy.DreamLive(filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", "settings.json"))
		},
		Secrets: func() []string {
			if tok, err := readPrivate(apiTokenFile()); err == nil && tok != "" {
				return []string{tok}
			}
			return nil
		},
	}
	log.Printf("memory tidy: nightly from %02d:00 on %s", hour, model)
	go t.Run(ctx)
}
