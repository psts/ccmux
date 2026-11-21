package api

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// An agent's earlier conversations, read only. A base that starts fresh
// opens a new conversation at every wake, so its session's own
// conversation is only the latest; the ones before are still in Claude
// Code's store for the agent's folder. The thread page lists them and shows
// one in place of the current conversation (threadpast.js). Nothing here
// changes what the agent runs or remembers.
//
//	GET /v1/sessions/{id}/conversations         the list, newest first
//	GET /v1/sessions/{id}/conversations/{conv}  one, as a snapshot frame
//
// Only an agent's session lists them here: a repo's folder holds every
// session that ever ran in it (its threads, its tmux panes), which is not
// one session's history. Any other session reads only the conversations
// its own clean starts left (store/cleanstart.go), which the page shows
// above the one it runs now.

// pastLimit bounds the list: a daily agent has a conversation a day.
const pastLimit = 200

// convIDRe is a conversation id as Claude Code makes them; anything else
// is refused before it reaches the sidecar's arguments.
var convIDRe = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

// pastConversation is one entry of the list.
type pastConversation struct {
	ID      string `json:"id"`
	Title   string `json:"title,omitempty"`
	Updated int64  `json:"updated"`
	Current bool   `json:"current,omitempty"`
}

// agentSession is the agent session id names, or writes why not.
func (s *Server) agentSession(w http.ResponseWriter, id string) *store.RunnerSession {
	sess := s.pastSession(w, id)
	if sess != nil && sess.Kind != store.SessionAgent {
		writeError(w, http.StatusNotFound, "only an agent's session keeps its earlier conversations here")
		return nil
	}
	return sess
}

// pastSession is the session id names, when earlier conversations can be
// read on this daemon, or writes why not.
func (s *Server) pastSession(w http.ResponseWriter, id string) *store.RunnerSession {
	if !s.doorsReady(w) {
		return nil
	}
	if s.doors.Conversations == nil || s.doors.Past == nil {
		writeError(w, http.StatusServiceUnavailable, "earlier conversations cannot be read on this daemon")
		return nil
	}
	sess, err := s.runnerRows.RunnerSession(id)
	switch {
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "the session's row could not be read: "+err.Error())
		return nil
	case sess == nil:
		writeRunnerError(w, store.ErrNoSession)
		return nil
	}
	return sess
}

func (s *Server) listPastConversations(w http.ResponseWriter, r *http.Request) {
	sess := s.agentSession(w, r.PathValue("id"))
	if sess == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	list, err := s.doors.Conversations(ctx, *sess)
	if err != nil {
		writeError(w, http.StatusBadGateway, "the agent's conversations could not be read: "+err.Error())
		return
	}
	current := sess.ConversationID()
	out := make([]pastConversation, 0, min(len(list), pastLimit))
	for _, c := range list {
		if len(out) == pastLimit {
			break
		}
		out = append(out, pastConversation{ID: c.ID, Title: c.Title, Updated: c.Updated, Current: c.ID == current})
	}
	writeJSON(w, http.StatusOK, map[string]any{"current": current, "conversations": out})
}

func (s *Server) pastConversation(w http.ResponseWriter, r *http.Request) {
	conv := r.PathValue("conv")
	if !convIDRe.MatchString(conv) {
		writeError(w, http.StatusBadRequest, "not a conversation id: "+conv)
		return
	}
	sess := s.pastSession(w, r.PathValue("id"))
	if sess == nil {
		return
	}
	if sess.Kind != store.SessionAgent && !sess.HasLeft(conv) {
		writeError(w, http.StatusNotFound, "not a conversation this session left")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	msgs, err := s.doors.Past(ctx, *sess, conv)
	if err != nil {
		writeError(w, http.StatusBadGateway, "the conversation could not be read: "+err.Error())
		return
	}
	data, err := jsonMessages(msgs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, streamFrame{T: "snapshot", State: "past", Data: data})
}
