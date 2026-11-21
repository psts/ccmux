package sdkupdate

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

// A real trial, run only when asked: it starts a runner and pays for a
// Haiku turn.
//
//	CCMUX_SDK_LIVE=<a sidecar's serve.mjs> go test ./internal/sdkupdate -run TestLiveTrial
//
// It must pass on the host's claude and fail on one that cannot run.
func TestLiveTrial(t *testing.T) {
	script := os.Getenv("CCMUX_SDK_LIVE")
	if script == "" {
		t.Skip("set CCMUX_SDK_LIVE to a sidecar script to run a real trial")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("no claude")
	}
	try := TrialSession(node, os.Environ, TrialTag(t.TempDir()))
	if err := try(context.Background(), script, claude); err != nil {
		t.Fatalf("a trial on the host's claude failed: %v", err)
	}
	if err := try(context.Background(), script, "/bin/false"); err == nil {
		t.Fatal("a trial on a claude that cannot run passed")
	}
}
