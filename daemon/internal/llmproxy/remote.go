package llmproxy

import (
	"context"
	"log"
)

// Remote is the proxy when it runs as its own service (internal/llmserve),
// seen from the daemon. Account health is learned from traffic, and traffic
// goes through the service, so the four reads that depend on health are the
// service's to answer: the daemon's own copy has seen nothing.
//
// Everything else stays local on both sides. Settings are read from the same
// database by each, and the daemon alone writes them.
type Remote interface {
	Statuses(ctx context.Context) ([]AccountStatus, error)
	PaneOrders(ctx context.Context, paneIDs []string) (orders map[string][]string, failures map[string]string, err error)
	PaneStatus(ctx context.Context, paneID string) (explicit string, order []string, err error)
	CheckAccount(ctx context.Context, name string) (AccountStatus, error)
}

// SetRemote hands the health-based reads to a proxy service. nil (the
// default) answers them here, which is right when the proxy runs in this
// process.
func (s *Service) SetRemote(r Remote) { s.remote = r }

// remoteDown logs a read the service did not answer. The three reads fall
// back to this process's own answer: accounts and routes are still right (same
// database), only the health behind them is missing, and a settings page that
// shows every account as untried is better than one that will not load.
func remoteDown(what string, err error) {
	log.Printf("llm: the proxy service did not answer %s, answering without its account health: %v", what, err)
}
