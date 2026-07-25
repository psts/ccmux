package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
)

// CcmuxTag is the ACL tag a tailnet node must carry to be discovered as a member
// host. The hub node itself is always a member (it's also a host), tagged or not.
const CcmuxTag = "tag:ccmux"

// TailnetDiscoverer builds the registry's discover func from a tsnet LocalClient:
// it reads the node status and returns self plus every peer carrying CcmuxTag.
func TailnetDiscoverer(ctx context.Context, lc *local.Client) func() ([]Node, error) {
	return func() ([]Node, error) {
		st, err := lc.Status(ctx)
		if err != nil {
			return nil, err
		}
		return nodesFromStatus(st, CcmuxTag), nil
	}
}

// HTTPProber builds the registry's probe func: GET <baseURL>/v1/health with a
// per-request timeout. client MUST route the tailnet — pass tsnet.Server.HTTPClient()
// so MagicDNS names resolve and dials go over the tailnet, not the host's default
// network.
func HTTPProber(client *http.Client, timeout time.Duration) func(baseURL string) (Health, error) {
	return func(baseURL string) (Health, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/health", nil)
		if err != nil {
			return Health{}, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return Health{}, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return Health{}, fmt.Errorf("health returned %d", resp.StatusCode)
		}
		var h Health
		if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
			return Health{}, fmt.Errorf("decode health: %w", err)
		}
		return h, nil
	}
}

// nodesFromStatus turns a tailnet status into ccmux member nodes: Self (always,
// the hub is a host) plus every peer carrying wantTag. A node without a DNS name
// is skipped (nothing dialable). Offline peers are kept — the probe decides
// reachability, not the control-plane snapshot.
func nodesFromStatus(st *ipnstate.Status, wantTag string) []Node {
	var out []Node
	if ps := st.Self; ps != nil && ps.DNSName != "" {
		out = append(out, nodeOf(ps))
	}
	for _, ps := range st.Peer {
		if ps != nil && ps.DNSName != "" && hasTag(peerTags(ps), wantTag) {
			out = append(out, nodeOf(ps))
		}
	}
	return out
}

func nodeOf(ps *ipnstate.PeerStatus) Node {
	addr := strings.TrimSuffix(ps.DNSName, ".")
	return Node{ID: magicDNSLabel(ps.DNSName), Addr: addr}
}

// magicDNSLabel extracts the first label of a MagicDNS FQDN (which ends with a
// dot): "hostb.tailb9053d.ts.net." → "hostb".
func magicDNSLabel(dnsName string) string {
	name := strings.TrimSuffix(dnsName, ".")
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}

// hasTag reports whether tags contains want.
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// peerTags flattens a PeerStatus.Tags view to a slice (nil-safe).
func peerTags(ps *ipnstate.PeerStatus) []string {
	if ps == nil || ps.Tags == nil {
		return nil
	}
	return ps.Tags.AsSlice()
}
