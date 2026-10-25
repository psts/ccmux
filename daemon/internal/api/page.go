package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/hooktrace"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/push"
)

// pageStore keeps the one-page-per-incident rule (store/pages.go). It rides
// on the push store: EnablePush picks it up when the store has it.
type pageStore interface {
	ClaimPage(scope, incident, byName string, now int64) (claimed bool, pagedAt int64, pagedBy string, err error)
	ReleasePage(scope, incident string) error
}

const (
	maxIncidentLen = 80
	maxPageLen     = 300
)

type pageReq struct {
	Incident string `json:"incident"`
	Message  string `json:"message"`
}

// panePage: POST /v1/panes/{id}/page {incident, message} — the page tool.
// An agent (or any hosted session) sends its human one Web Push, urgent,
// even while a lens is focused, and at most once per incident key in its
// window. Gated like every pane route that acts as the pane: loopback AND
// the pane's own token. Answers {sent} with the number of devices reached;
// 409 when the incident was paged already.
func (s *Server) panePage(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) || !s.peersEnabled(w) {
		return
	}
	paneID := r.PathValue("id")
	if tok := bearerToken(r); tok == "" || !s.peersSvc.AuthorizePane(paneID, tok) {
		writeError(w, http.StatusUnauthorized, "pane token required (CCMUX_PANE_TOKEN in the pane's environment)")
		return
	}
	if s.pages == nil || s.sender == nil {
		writeError(w, http.StatusServiceUnavailable, "push is not set up on this daemon, so it cannot page")
		return
	}
	var req pageReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg := req.check(); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	status, body := s.page(r.Context(), paneID, req)
	writeJSON(w, status, body)
}

func (req *pageReq) check() string {
	req.Incident, req.Message = strings.TrimSpace(req.Incident), strings.TrimSpace(req.Message)
	switch {
	case req.Incident == "" || len(req.Incident) > maxIncidentLen:
		return fmt.Sprintf("incident is required: a short key for this one incident, at most %d characters", maxIncidentLen)
	case req.Message == "":
		return "message is required: one line saying what is wrong"
	}
	if len(req.Message) > maxPageLen {
		req.Message = req.Message[:maxPageLen]
	}
	return ""
}

// page claims the incident for the pane's window, then pushes. A page that
// reached no device gives its claim back, so the next try is a first page.
func (s *Server) page(ctx context.Context, paneID string, req pageReq) (int, any) {
	wsID := s.mgr.WorkspaceForPane(paneID)
	ws := s.mgr.Workspace(wsID)
	if ws == nil {
		return http.StatusNotFound, map[string]string{"error": "unknown pane"}
	}
	scope, from := pageScope(s, paneID, ws), pageSender(paneID, ws)
	claimed, at, by, err := s.pages.ClaimPage(scope, req.Incident, from, time.Now().UnixMilli())
	if err != nil {
		return http.StatusInternalServerError, map[string]string{"error": err.Error()}
	}
	if !claimed {
		return http.StatusConflict, map[string]string{"error": fmt.Sprintf(
			"incident %q was already paged by %s at %s; one page per incident. Use a new key only for a new incident",
			req.Incident, by, time.UnixMilli(at).Local().Format("2006-01-02 15:04"))}
	}
	sent := s.pushPage(ctx, wsID, from, req)
	if sent == 0 {
		msg := "no device took the page (nobody is subscribed to push, or every send failed)"
		if err := s.pages.ReleasePage(scope, req.Incident); err != nil {
			// Said, because the next try for this incident will now be refused.
			msg += "; and the incident is still marked paged (" + err.Error() + "), so a retry needs a new incident key"
			tracePage(wsID, "claim-kept", "", "release failed: "+err.Error())
		}
		return http.StatusServiceUnavailable, map[string]string{"error": msg}
	}
	return http.StatusOK, map[string]int{"sent": sent}
}

// pageScope is the window a pane works in, or its own session outside one.
func pageScope(s *Server, paneID string, ws *model.Workspace) string {
	if group, ok := s.mgr.GroupForPane(paneID); ok && group != "" {
		return group
	}
	return ws.ID
}

// pageSender names who pages: the agent, else the session.
func pageSender(paneID string, ws *model.Workspace) string {
	for _, p := range ws.Panes {
		if p.ID == paneID && p.Agent != "" {
			return p.Agent
		}
	}
	return orDefault(ws.Name, ws.ID)
}

// pushPage sends one page to every subscribed device of the workspace's
// audience. Unlike an attention push it ignores focus: a page is for when
// it matters even if the human is at a screen. Its own tag and topic, so it
// neither replaces nor is replaced by the session's attention alerts.
func (s *Server) pushPage(ctx context.Context, wsID, from string, req pageReq) int {
	subs, err := s.pushStore.ListPushSubscriptions()
	if err != nil {
		tracePage(wsID, "no-push", "", "list subscriptions: "+err.Error())
		return 0
	}
	audience, bounded := s.alertAudience(wsID)
	body, _ := json.Marshal(pushPayload{Title: from + " pages you", Body: req.Message,
		Tag: "page-" + req.Incident, URL: "/?ws=" + wsID, State: "page"})
	topic := pageTopic(wsID, req.Incident)
	sent := 0
	for _, sub := range subs {
		if bounded && !audience[sub.Login] {
			continue
		}
		status, err := s.sender.Send(ctx, sub.Address, body, topic)
		switch {
		case err != nil:
			tracePage(wsID, "send-failed", sub.Login, err.Error())
		case push.Dead(status):
			_ = s.pushStore.DeletePushSubscription(sub.ID)
			tracePage(wsID, "pruned", sub.Login, "push service says gone")
		case status < 200 || status > 299:
			// Answered but not taken (a bad key, a rate limit, a 5xx): not a page.
			tracePage(wsID, "rejected", sub.Login, fmt.Sprintf("push service answered %d", status))
		default:
			sent++
			tracePage(wsID, "sent", sub.Login, "page "+req.Incident)
		}
	}
	return sent
}

// pageTopic is a push topic of its own per incident (base64url-safe, at
// most 32 characters, as the push service wants).
func pageTopic(wsID, incident string) string {
	sum := sha256.Sum256([]byte(wsID + "\x00" + incident))
	return "pg" + hex.EncodeToString(sum[:15])
}

func tracePage(wsID, decision, login, detail string) {
	hooktrace.Write(hooktrace.Line{Stage: hooktrace.StagePush, WorkspaceID: wsID,
		Attention: "page", Decision: decision, Login: login, Detail: detail})
}
