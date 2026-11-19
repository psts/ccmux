package llmserve

import (
	"encoding/json"
	"errors"
	"net/http"

	"ccmux.dev/ccmuxd/internal/llmproxy"
)

// The control routes: the four reads the daemon asks the service for
// (llmproxy.Remote), because they depend on account health and health is
// learned from the traffic that flows here. client.go is their other end,
// and the two change together: both are in the hashed code, so a daemon
// never talks to a service built from another version of this file for
// longer than the restart it orders.

type control struct{ svc *llmproxy.Service }

func (c control) mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /ctl/statuses", c.statuses)
	mux.HandleFunc("POST /ctl/orders", c.orders)
	mux.HandleFunc("GET /ctl/panes/{pane}", c.pane)
	mux.HandleFunc("POST /ctl/accounts/{name}/check", c.check)
}

// wireError is every failed control answer. Kind carries the two errors the
// daemon's API tells apart by status (404 and 400): an error's identity does
// not cross a process boundary by itself.
type wireError struct {
	Error string `json:"error"`
	Kind  string `json:"kind,omitempty"`
}

const (
	kindUnknownAccount = "unknown-account"
	kindNotCheckable   = "not-checkable"
)

func fail(w http.ResponseWriter, err error) {
	we := wireError{Error: err.Error()}
	switch {
	case errors.Is(err, llmproxy.ErrUnknownAccount):
		we.Kind = kindUnknownAccount
	case errors.Is(err, llmproxy.ErrNotCheckable):
		we.Kind = kindNotCheckable
	}
	writeJSON(w, http.StatusBadGateway, we)
}

func (c control) statuses(w http.ResponseWriter, _ *http.Request) {
	sts, err := c.svc.Statuses()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sts)
}

type ordersRequest struct {
	Panes []string `json:"panes"`
}

type ordersAnswer struct {
	Orders   map[string][]string `json:"orders"`
	Failures map[string]string   `json:"failures"`
}

func (c control) orders(w http.ResponseWriter, r *http.Request) {
	var req ordersRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	orders, failures, err := c.svc.PaneOrders(req.Panes)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ordersAnswer{Orders: orders, Failures: failures})
}

type paneAnswer struct {
	Explicit string   `json:"explicit"`
	Order    []string `json:"order"`
}

func (c control) pane(w http.ResponseWriter, r *http.Request) {
	explicit, order, err := c.svc.PaneStatus(r.PathValue("pane"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, paneAnswer{Explicit: explicit, Order: order})
}

func (c control) check(w http.ResponseWriter, r *http.Request) {
	st, err := c.svc.CheckAccount(r.Context(), r.PathValue("name"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
