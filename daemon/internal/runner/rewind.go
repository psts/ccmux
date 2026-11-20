package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"ccmux.dev/ccmuxd/internal/store"
	"github.com/google/uuid"
)

// Going back to an earlier prompt (the thread page's "Edit"). The runner
// does the work it can do alone (claudeserve/rewind.mjs): it puts back the
// files Claude changed since that prompt and forks the conversation to
// just before it. The supervisor then makes the fork the session's
// conversation and starts the session again on it.

// RewindFiles is what undoing the files does (or would do): Files are the
// paths, and CanRewind false says why not in Error (a prompt from before
// the runner kept checkpoints, say). The conversation goes back either way.
type RewindFiles struct {
	CanRewind bool     `json:"canRewind"`
	Error     string   `json:"error"`
	Files     []string `json:"files"`
}

// Rewind is a runner's answer: how many prompts go (the one gone back to
// included), the files, and on a real going back the conversation the
// session starts again on.
type Rewind struct {
	Dropped      int         `json:"dropped"`
	Files        RewindFiles `json:"files"`
	Conversation string      `json:"conversation,omitempty"`
}

// ErrRewindRefused is a going back the runner said no to, in its words:
// Claude is working, the message is not a prompt of this conversation.
var ErrRewindRefused = errors.New("refused")

// Rewind asks the runner to look at (dry) or go back to prompt `to`.
func (c *Client) Rewind(ctx context.Context, to string, dry bool) (Rewind, error) {
	var out Rewind
	b, _ := json.Marshal(map[string]any{"to": to, "dry": dry})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://runner/rewind", bytes.NewReader(b))
	if err != nil {
		return out, err
	}
	req.Header.Set("content-type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusConflict {
		var said struct{ Error string }
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&said)
		return out, fmt.Errorf("%w: %s", ErrRewindRefused, said.Error)
	}
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return out, fmt.Errorf("runner POST /rewind: HTTP %d: %s", res.StatusCode, bytes.TrimSpace(msg))
	}
	return out, json.NewDecoder(res.Body).Decode(&out)
}

// Rewind looks at (dry) or goes back to a running session's earlier prompt
// `to`. Going back marks every prompt the runner already has as delivered
// first, so none of the ones cut away is sent again to the fork; then the
// runner goes back, the fork becomes the session's conversation, and the
// session is started again on it. Prompts that come in meanwhile wait in
// the inbox for the new start.
func (s *Supervisor) Rewind(ctx context.Context, sessionID, to string, dry bool) (Rewind, error) {
	defer s.lock(sessionID)()
	sess, err := s.row(sessionID)
	if err != nil {
		return Rewind{}, err
	}
	if sess.Wanted != store.SessionRunning {
		return Rewind{}, fmt.Errorf("%w: the session is not running; wake it first", ErrRewindRefused)
	}
	c := s.dial(s.paths.Socket(sessionID))
	if dry {
		return c.Rewind(ctx, to, true)
	}
	if _, _, err := s.markHeld(ctx, c, sessionID); err != nil {
		return Rewind{}, err
	}
	// Not cut short by the caller leaving: once asked, the runner goes
	// back whether or not anyone waits for the answer.
	out, err := c.Rewind(context.WithoutCancel(ctx), to, false)
	if err != nil {
		if errors.Is(err, ErrRewindRefused) {
			return out, err
		}
		s.restartSoon(sessionID, err)
		return out, fmt.Errorf("the runner may have put files back before its answer was lost (%w); it starts again in a moment on the conversation it left", err)
	}
	if err := s.land(sess, out.Conversation); err != nil {
		s.restartSoon(sessionID, err)
		return out, fmt.Errorf("the runner went back (files put back where it could), but %w; it starts again in a moment", err)
	}
	s.announce(sessionID)
	return out, nil
}

// restartSoon has a reconcile restart the session between turns. A runner
// that went back takes no prompt until it starts again, so after any
// failure past the ask it is restarted: on the fork when the row moved,
// else on the conversation it left.
func (s *Supervisor) restartSoon(sessionID string, why error) {
	s.due.Store(sessionID, struct{}{})
	s.Kick()
	log.Printf("runner: session %s: going back failed (%v); restarting it at the next pass", short(sessionID), why)
}

// land makes the runner's fork the session's conversation and starts the
// session again on it. A conversation that is not a uuid is never saved:
// it ends up in a file path (Paths.StateFile).
func (s *Supervisor) land(sess *store.RunnerSession, conv string) error {
	if _, err := uuid.Parse(conv); err != nil {
		return fmt.Errorf("its new conversation %q is not an id", conv)
	}
	if err := s.st.RotateRunnerConversation(sess.ID, conv, s.millis()); err != nil {
		return fmt.Errorf("its new conversation was not saved: %w", err)
	}
	sess.Conversation, sess.FreshNext = conv, false
	log.Printf("runner: session %s: went back to an earlier prompt; starts again on %s", short(sess.ID), short(conv))
	if err := s.restart(*sess); err != nil {
		return fmt.Errorf("it did not start again: %w", err)
	}
	return nil
}
