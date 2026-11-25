package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/api"
	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/memtidy"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/todos"
)

// tokenWarn says once that the API token is not checked for.
var tokenWarn sync.Once

// memoryTidyLimit is how long one repo's tidy-up may take.
const memoryTidyLimit = 30 * time.Minute

// startMemoryTidy runs the nightly tidy-up of each repo's Claude Code
// memory (internal/memtidy). A scratch daemon runs none unless
// -memory-tidy-hour is given: a test must not rewrite the person's memory.
// The per-repo switches are served either way; where none runs, the
// lens is told so.
func startMemoryTidy(ctx context.Context, st *store.SQLite, srv *api.Server, hour int, model string) {
	home, _ := os.UserHomeDir()
	stateDir := filepath.Join(filepath.Dir(runnerStateDir()), "memory-tidy")
	keys := &todos.Keys{}
	sw := &memtidy.Switches{Settings: st, Key: keys.Of, StateDir: stateDir}
	if scratchRoot() != "" && !flagGiven("memory-tidy-hour") {
		hour = -1
	}
	if hour < 0 || hour > 23 {
		srv.SetMemoryTidy(sw, false)
		log.Printf("memory tidy: off")
		return
	}
	claude, err := harness.LookPath("claude")
	if err != nil {
		srv.SetMemoryTidy(sw, false)
		log.Printf("memory tidy: off, no claude on this host: %v", err)
		return
	}
	srv.SetMemoryTidy(sw, true)
	t := &memtidy.Tidier{
		Sessions: st.RunnerSessions,
		Projects: filepath.Join(home, ".claude", "projects"),
		StateDir: stateDir,
		Hour:     hour,
		Claude:   memtidy.Headless(claude, model, append(runnerEnv(), "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1"), memoryTidyLimit),
		Enabled:  sw.Enabled,
		Todo: func(repo, text, urgency string) error {
			return srv.AddTodo(repo, text, urgency, "memory tidy-up")
		},
		DreamOn: func() bool {
			return memtidy.DreamLive(filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", "settings.json"))
		},
		Secrets: memoryTidySecrets,
	}
	log.Printf("memory tidy: nightly from %02d:00 on %s", hour, model)
	go t.Run(ctx)
}

// memoryTidySecrets are the values never written into memory or a to-do:
// the daemon's API token.
func memoryTidySecrets() []string {
	tok, err := readPrivate(apiTokenFile())
	if err != nil || tok == "" {
		tokenWarn.Do(func() { log.Printf("memory tidy: the API token could not be read, so it is not checked for: %v", err) })
		return nil
	}
	return []string{tok}
}
