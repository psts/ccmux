// Delegation tasks: a tracked send plus worker status reporting.
//
// The transcripts show what untracked delegation costs: "please ack" text
// conventions, chase-up pings, nudge loops, and a human hand-carrying status
// between sessions. A delegation here is an ordinary bus message plus one
// durable row; the worker reports transitions with update_task and the
// delegator hears about them automatically through its own durable queue —
// the same replay-and-ack path every other message survives restarts by.
package peers

import (
	"fmt"
	"regexp"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// taskStatuses are the worker-reportable transitions. "sent" is set by the
// bus at creation and is not reportable.
var taskStatuses = map[string]bool{
	"acked": true, "working": true, "completed": true, "failed": true,
}

// taskHeader opens every delegation message so the worker knows it holds a
// tracked task and how to report on it. Running sessions pattern-match on the
// bracketed prefix; change it only with the shim instructions that teach it.
func taskHeader(taskID string) string {
	return fmt.Sprintf("[claude-peers delegation task %s] This message is a tracked delegation. "+
		"You are the worker: call update_task(task_id=%q, status=\"acked\") now, "+
		"\"working\" when you start, and close with status=\"completed\" plus a result "+
		"(or \"failed\" and why). The delegator sees your updates automatically.",
		taskID, taskID)
}

// DelegateReq mirrors the delegate tool's arguments — SendReq plus nothing:
// the task envelope is the bus's, not the caller's.
type DelegateReq struct {
	FromID         string `json:"from_id"`
	ToID           string `json:"to_id"`
	ToName         string `json:"to_name"`
	ToGroup        string `json:"to_group"`
	Text           string `json:"text"`
	SpawnIfMissing bool   `json:"spawn_if_missing"`
	ToRepo         string `json:"to_repo"`
}

// DelegateResp is the tool-facing outcome. Same envelope semantics as
// SendResp, plus the task id the delegator will hear updates under.
type DelegateResp struct {
	OK       bool   `json:"ok"`
	TaskID   string `json:"task_id,omitempty"`
	Spawning bool   `json:"spawning,omitempty"`
	Queued   bool   `json:"queued,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Delegate routes one tracked delegation: create the task row, then deliver
// the headered message through the ordinary send path (including the spawn
// queue, whose queued text carries the header to a worker that doesn't exist
// yet — its first update_task claims the row).
func (s *Service) Delegate(req DelegateReq) DelegateResp {
	s.mu.Lock()
	defer s.mu.Unlock()

	sender := s.peers[req.FromID]
	if sender == nil {
		return DelegateResp{Error: "sender is not registered"}
	}
	senderGroup := s.groupOfLocked(sender)
	taskID := "tsk_" + randomID()
	sreq := SendReq{FromID: req.FromID, ToID: req.ToID, ToName: req.ToName,
		ToGroup: req.ToGroup, Text: taskHeader(taskID) + "\n\n" + req.Text,
		SpawnIfMissing: req.SpawnIfMissing, ToRepo: req.ToRepo}

	target, resp := s.resolveTargetLocked(sreq, sender, senderGroup)
	if target == nil {
		if !resp.Spawning {
			return DelegateResp{Error: resp.Error}
		}
		if err := s.saveTaskLocked(taskID, sender.ID, "", senderGroup, req.Text); err != nil {
			return DelegateResp{Error: err.Error()}
		}
		return DelegateResp{OK: true, TaskID: taskID, Spawning: true}
	}
	if err := s.checkReachableLocked(sreq, sender, senderGroup, target); err != "" {
		return DelegateResp{Error: err}
	}
	if targetGroup := s.groupOfLocked(target); !sameGroup(targetGroup, senderGroup) {
		s.grantReplyLocked(target.ID, sender.ID)
	}
	if err := s.saveTaskLocked(taskID, sender.ID, target.ID, senderGroup, req.Text); err != nil {
		return DelegateResp{Error: err.Error()}
	}
	if err := s.deliverLocked(s.eventFromLocked(sender, target, senderGroup, sreq.Text)); err != nil {
		_ = s.st.DeletePeerTask(taskID) // no message out, no row to report against
		return DelegateResp{Error: err.Error()}
	}
	return DelegateResp{OK: true, TaskID: taskID, Queued: !s.presentLocked(target)}
}

func (s *Service) saveTaskLocked(taskID, fromID, toID, group, text string) error {
	now := s.Now().UnixMilli()
	return s.st.SavePeerTask(store.PeerTask{TaskID: taskID, FromID: fromID, ToID: toID,
		Group: group, Text: text, Status: "sent", CreatedAt: now, UpdatedAt: now})
}

// TaskUpdateReq mirrors the update_task tool's arguments; PeerID is the
// reporting worker.
type TaskUpdateReq struct {
	PeerID  string `json:"peer_id"`
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Result  string `json:"result"`
}

// TaskUpdateResp is the tool-facing outcome.
type TaskUpdateResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// UpdateTask records a status report and forwards it to the other side's
// queue. Neither side need be registered right now — the mailboxes are
// durable, which is the entire point. The delivery happens BEFORE the row is
// saved: a saved-but-undelivered terminal status would be unrecoverable (the
// closed guard rejects the retry), while a delivered-but-unsaved one merely
// lets a retry send a duplicate update.
func (s *Service) UpdateTask(req TaskUpdateReq) TaskUpdateResp {
	s.mu.Lock()
	defer s.mu.Unlock()

	reporter := s.peers[req.PeerID]
	if reporter == nil {
		return TaskUpdateResp{Error: "peer is not registered"}
	}
	if !taskStatuses[req.Status] {
		return TaskUpdateResp{Error: `status must be one of "acked", "working", "completed", "failed"`}
	}
	t, errText := s.claimTaskLocked(req.TaskID, reporter, req.Status)
	if errText != "" {
		return TaskUpdateResp{Error: errText}
	}
	t.Status, t.StatusMessage = req.Status, req.Message
	if req.Result != "" {
		t.Result = req.Result
	}
	t.UpdatedAt = s.Now().UnixMilli()
	if to := taskCounterparty(t, reporter.ID); to != "" {
		if err := s.deliverLocked(s.taskUpdateEventLocked(reporter, to, t)); err != nil {
			return TaskUpdateResp{Error: err.Error()}
		}
	}
	if err := s.st.SavePeerTask(*t); err != nil {
		return TaskUpdateResp{Error: err.Error()}
	}
	return TaskUpdateResp{OK: true}
}

// claimTaskLocked loads the task and decides whether reporter may report
// status on it. Returns the row (possibly claimed) or an error string.
func (s *Service) claimTaskLocked(taskID string, reporter *Peer, status string) (*store.PeerTask, string) {
	t, err := s.st.PeerTask(taskID)
	if err != nil {
		return nil, err.Error()
	}
	if t == nil {
		return nil, fmt.Sprintf("unknown task %s", taskID)
	}
	if t.Status == "completed" || t.Status == "failed" {
		return nil, fmt.Sprintf("task %s is already closed (%s)", t.TaskID, t.Status)
	}
	return authorizeTaskReport(t, reporter.ID, status)
}

// authorizeTaskReport is the pure decision: the bound worker always reports;
// the first non-delegator reporter claims an unbound (spawn-pending) row; the
// delegator may only CLOSE its own task (cancel a delegation nobody will ever
// answer).
func authorizeTaskReport(t *store.PeerTask, reporterID, status string) (*store.PeerTask, string) {
	switch {
	case t.ToID == reporterID:
		return t, ""
	case t.ToID == "" && t.FromID != reporterID:
		t.ToID = reporterID // spawn_if_missing worker claiming its delegation
		return t, ""
	case t.FromID == reporterID && (status == "completed" || status == "failed"):
		return t, "" // delegator closing its own task
	case t.FromID == reporterID:
		return nil, fmt.Sprintf("task %s is yours to receive updates on — only its worker reports progress (you may close it with completed/failed)", t.TaskID)
	default:
		return nil, fmt.Sprintf("task %s was not delegated to this peer", t.TaskID)
	}
}

// taskCounterparty is who hears about a report: the other end of the task,
// or nobody when a delegator closes a task whose worker never existed.
func taskCounterparty(t *store.PeerTask, reporterID string) string {
	if t.FromID == reporterID {
		return t.ToID
	}
	return t.FromID
}

// taskUpdateEventLocked renders a status report as a bus message to toID
// (the task's other side). Built by hand rather than via eventFromLocked
// because the recipient may be away: only its id is needed, its queue does
// the rest.
func (s *Service) taskUpdateEventLocked(worker *Peer, toID string, t *store.PeerTask) *model.PeerEvent {
	text := fmt.Sprintf("[claude-peers task update] %s: %s", t.TaskID, t.Status)
	if t.StatusMessage != "" {
		text += " — " + t.StatusMessage
	}
	if (t.Status == "completed" || t.Status == "failed") && t.Result != "" {
		text += "\n\nResult:\n" + t.Result
	}
	toName := ""
	if p := s.peers[toID]; p != nil {
		toName = p.Name
	}
	return &model.PeerEvent{
		Kind:   model.PeerEventMessage,
		FromID: worker.ID, FromName: worker.Name,
		FromSummary: worker.Summary, FromCWD: worker.CWD,
		ToID: toID, ToName: toName,
		Group: t.Group, Text: text,
	}
}

// failTasksInLocked closes every open task carried by a failed spawn's queued
// requests, so a delegation whose worker will never exist doesn't sit in the
// delegator's open list forever. The task id is recovered from the queued
// text's header — the one place it exists once the request is in the spawn
// queue. Best-effort: rows that fail to save are retried by nobody, but the
// requester notification (sent by the callers) names the failure regardless.
func (s *Service) failTasksInLocked(pending *pendingSpawn, why string) {
	for _, req := range pending.requests {
		m := taskHeaderRe.FindStringSubmatch(req.text)
		if m == nil {
			continue // plain send_message spawn, no task attached
		}
		t, err := s.st.PeerTask(m[1])
		if err != nil || t == nil || t.Status == "completed" || t.Status == "failed" {
			continue
		}
		t.Status, t.StatusMessage, t.UpdatedAt = "failed", why, s.Now().UnixMilli()
		_ = s.st.SavePeerTask(*t)
	}
}

// taskHeaderRe recovers a delegation's task id from its message header
// (the inverse of taskHeader).
var taskHeaderRe = regexp.MustCompile(`^\[claude-peers delegation task (tsk_[a-z0-9]+)\]`)

// OpenTasks lists the peer's non-terminal delegations, both directions —
// the check_messages enrichment that survives a delegator's own restart.
func (s *Service) OpenTasks(peerID string) ([]store.PeerTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peers[peerID] == nil {
		return nil, fmt.Errorf("peer %s is not registered", peerID)
	}
	return s.st.OpenPeerTasksFor(peerID, 20)
}
