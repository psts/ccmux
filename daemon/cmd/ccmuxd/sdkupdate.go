package main

import (
	"context"
	"flag"
	"log"
	"path/filepath"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/sdkupdate"
)

// startSDKUpdate runs the daily SDK update (internal/sdkupdate). A scratch
// daemon (CCMUXD_ROOT) takes none unless -sdk-update-every says so: a
// test must never move a version under the person's sessions by surprise,
// nor pay for trial turns it did not ask for.
func startSDKUpdate(ctx context.Context, agents *agent.Store, every time.Duration) {
	if scratchRoot() != "" && !flagGiven("sdk-update-every") {
		every = 0
	}
	if every <= 0 {
		log.Printf("sdk update: off")
		return
	}
	node, err := harness.LookPath("node")
	if err != nil {
		log.Printf("sdk update: off, no node on this host: %v", err)
		return
	}
	claude, _ := harness.LookPath("claude") // none: the trial runs on the SDK's own only
	tag := sdkupdate.TrialTag(agents.Root)
	u := &sdkupdate.Updater{
		Store: agents, Every: every, Latest: agent.LatestSDKOnNPM, Claude: claude,
		Trial: sdkupdate.TrialSession(node, runnerEnv, tag), InUse: specScripts,
		Cleanup: func() { sdkupdate.CleanTrials(tag); agents.CleanSDKInstalls() },
	}
	go u.Run(ctx)
}

// flagGiven says whether name was on the command line.
func flagGiven(name string) bool {
	given := false
	flag.Visit(func(f *flag.Flag) { given = given || f.Name == name })
	return given
}

// specScripts is the sidecar every session's spec names, so pruning old
// SDK versions never removes one a session runs from.
func specScripts() []string {
	files, _ := filepath.Glob(filepath.Join(runnerStateDir(), "*.json"))
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, ".state.json") {
			continue
		}
		if sp, err := runner.ReadSpec(f); err == nil {
			out = append(out, sp.Script)
		}
	}
	return out
}
