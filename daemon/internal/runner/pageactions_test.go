package runner

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The page's mode switch and task stop reach the runner's own routes, and
// only the runner's worded "no" reads as a refusal.
func TestModeAndTaskStopReachTheRunnerAndOnlyItsNoIsARefusal(t *testing.T) {
	r := newRig(t)
	sess, _ := r.sup.Create(NewSession{Cwd: "/r/app"})
	r.reconcile()
	fr := r.run[sess.ID]
	c := r.sup.Runner(sess.ID)
	ctx := context.Background()
	if err := c.SetMode(ctx, "plan"); err != nil {
		t.Fatal(err)
	}
	if err := c.StopTask(ctx, "b 1"); err != nil {
		t.Fatal(err)
	}
	if want := []string{`/mode {"mode":"plan"}`, "/task/b 1/stop {}"}; !slices.Equal(fr.actions, want) {
		t.Fatalf("what the runner was sent: %q", fr.actions)
	}
	fr.actionCode, fr.actionSaid = http.StatusConflict, "not a mode to switch to: x"
	if err := c.SetMode(ctx, "x"); !errors.Is(err, ErrRunnerSaidNo) || !strings.HasSuffix(err.Error(), ": not a mode to switch to: x") {
		t.Fatalf("a no, in the runner's words: %v", err)
	}
	fr.actionCode, fr.actionSaid = http.StatusInternalServerError, "Claude Code exited"
	if err := c.StopTask(ctx, "b1"); err == nil || errors.Is(err, ErrRunnerSaidNo) || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("a failure is not a refusal: %v", err)
	}
	fr.actionCode, fr.actionSaid = http.StatusConflict, ""
	if err := c.StopTask(ctx, "b1"); err == nil || errors.Is(err, ErrRunnerSaidNo) {
		t.Fatalf("a 409 with no words is not read as the runner's no: %v", err)
	}
}
