package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	"ccmux.dev/ccmuxd/internal/model"
)

// Client talks to member hosts over the tailnet. transport MUST route the
// tailnet (built from tsnet.Server.Dial) — it's shared by workspace fetches and
// the reverse proxy so every hop resolves MagicDNS and validates the host's
// ts.net cert (SNI = the host's name).
type Client struct {
	transport http.RoundTripper
	http      *http.Client
}

// NewClient wraps a tailnet-routing transport.
func NewClient(transport http.RoundTripper) *Client {
	return &Client{transport: transport, http: &http.Client{Transport: transport}}
}

// Transport is the tailnet-routing round-tripper, for callers that build their
// own timeout-bounded client (e.g. the hub's presence poller).
func (c *Client) Transport() http.RoundTripper { return c.transport }

// Workspaces fetches and decodes a host's GET /v1/workspaces (a RemoteFetcher).
func (c *Client) Workspaces(ctx context.Context, host Host) ([]*model.Workspace, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host.Addr+"/v1/workspaces", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("host %s workspaces: HTTP %d", host.ID, resp.StatusCode)
	}
	var wss []*model.Workspace
	if err := json.NewDecoder(resp.Body).Decode(&wss); err != nil {
		return nil, fmt.Errorf("host %s workspaces decode: %w", host.ID, err)
	}
	return wss, nil
}

// ReverseProxy forwards the inbound request to a member host, preserving method,
// path, query, and body. The path scheme is identical on hub and host, so the
// same path is the right target.
func (c *Client) ReverseProxy(host Host) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "https", Host: host.Addr}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = c.transport
	return rp
}
