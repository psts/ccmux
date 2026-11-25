package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

// What the peers bus needs of a runner session (peers/runnerpeer.go): its
// inbox, to hand it a message as a prompt, and its runner, to answer a card
// a delegator replied to. Both find the session through its pane.

// wirePeersRunners gives the bus its runner hooks once BOTH the supervisor
// and the peers service are there. Called from SetRunners and EnablePeers,
// so main's wiring order does not matter (as wirePeersAgents).
func (s *Server) wirePeersRunners() {
	if s.peersSvc == nil || s.runners == nil {
		return
	}
	s.peersSvc.RunnerInbox = s.runnerInbox
	s.peersSvc.ReplyToPane = s.replyToAgentPane // a runner's cards are answered through it too
	s.peersSvc.RunnerSummary = s.keepRunnerGoal
	s.peersSvc.StartMain = s.startMainForBus
}

// startMainForBus is the bus's StartMain: the main session of the repo
// session wsID, made when it has none, as the thread page makes one when a
// person opens a repo that has none. The bus then hands it the message
// that asked for it, which is its first prompt.
func (s *Server) startMainForBus(wsID string) (paneID, cwd string, err error) {
	ws := s.mgr.Workspace(wsID)
	if ws == nil || ws.Agent != "" {
		return "", "", fmt.Errorf("%s is not a repo session on this host", wsID)
	}
	lead, err := s.ensureLead(ws, "")
	if err != nil {
		return "", "", err
	}
	return lead.PaneID, lead.Cwd, nil
}

// keepRunnerGoal is the bus's RunnerSummary: the note a session gave the bus
// of what it is doing becomes its goal, kept on its row so the thread page
// has it after a daemon restart too.
func (s *Server) keepRunnerGoal(paneID, summary string) {
	info, ok := s.mgr.RunnerPane(paneID)
	if !ok {
		return
	}
	if err := s.runnerRows.SetRunnerSessionGoal(info.SessionID, summary, time.Now().UnixMilli()); err != nil {
		log.Printf("sessions: %s: its goal was not kept: %v", info.SessionID, err)
		return
	}
	s.sessionChanged(info.SessionID)
}

// runnerInbox is the bus's RunnerInbox: queue a message as a prompt for the
// session behind paneID, under the id the bus names it by. A session that
// has ended, or whose pane is gone, is ErrInboxClosed: nothing will read it.
func (s *Server) runnerInbox(paneID, promptID, text, sender string) error {
	info, ok := s.mgr.RunnerPane(paneID)
	if !ok {
		return peers.ErrInboxClosed
	}
	if held := s.agentWakeHeld(info.SessionID); held != "" {
		return errors.New(held) // stays in the mailbox; the bus hands it over again at its next sweep
	}
	_, err := s.runners.SendAs(info.SessionID, promptID, text, sender)
	if errors.Is(err, runner.ErrSessionDone) || errors.Is(err, store.ErrNoSession) {
		return peers.ErrInboxClosed
	}
	return err
}

// replyToRunner answers a card of a runner session with a delegator's
// verdict or answer. A card no longer pending (answered on the session's own
// page first, or gone with a restarted runner) is dropped with one line:
// the first answer wins.
func (s *Server) replyToRunner(paneID, sessionID string, reply peers.PaneReply) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	answered, err := s.runners.ReplyCard(ctx, sessionID, runner.CardReply{
		Kind: reply.Kind, ID: reply.RequestID, Allow: reply.Behavior == "allow", Answer: reply.Answer,
	})
	if err == nil && !answered {
		log.Printf("sessions: bus reply %s to pane %s: card no longer pending (answered on its page, or the session restarted)", reply.RequestID, paneID)
	}
	return err
}
