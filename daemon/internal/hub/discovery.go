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

// HubTag marks the designated hub node, so a member host can discover where to
// point its panes' peers bus (host-side hub discovery).
const HubTag = "tag:ccmux-hub"

// DiscoverHub returns the base URL of the tag:ccmux-hub node for a member host to
// federate its peers bus to, or "" when no hub is found or this node is itself
// the hub (selfID). A node must carry HubTag to be treated as the hub — Self is
// not force-included here (unlike member discovery).
func DiscoverHub(ctx context.Context, lc *local.Client, selfID string) string {
	st, err := lc.Status(ctx)
	if err != nil {
		return ""
	}
	return hubURLFromStatus(st, selfID)
}

// HubOwner returns the MagicDNS label of the tag:ccmux-hub node on the tailnet,
// or "" when no node carries the tag. Unlike DiscoverHub it does NOT exclude
// this node — the caller asking "who is the hub" is usually asking in order to
// compare it against itself (see DevDNSOwner), and a predicate built on
// DiscoverHub can't tell "I am the hub" from "there is no hub" since both
// answer "".
//
// The lowest label wins if several nodes carry the tag, so that every node in
// the fleet names the SAME owner. Answering "myself" wherever it is asked would
// make two tagged nodes both claim, which for a single-value shared resource
// means they overwrite each other forever.
//
// Offline and key-expired nodes still count. The hub is the only node that can
// serve the whole fleet's dev hostnames — it proxies each one to its owner — so
// a member seizing the record while the hub is down would not restore the fleet,
// it would replace one broken state with a narrower one that then flaps when the
// hub returns. The record waits for the hub.
func HubOwner(st *ipnstate.Status) string {
	if st == nil {
		return ""
	}
	owner := ""
	consider := func(ps *ipnstate.PeerStatus) {
		if ps == nil || ps.DNSName == "" || !hasTag(peerTags(ps), HubTag) {
			return
		}
		if label := magicDNSLabel(ps.DNSName); owner == "" || label < owner {
			owner = label
		}
	}
	consider(st.Self)
	for _, ps := range st.Peer {
		consider(ps)
	}
	return owner
}

// DevDNSOwner reports whether selfID may write the dev domain's wildcard A
// record — one record, one correct value, so exactly one writer. hubRole is
// whether this daemon runs with -hub. reason explains a refusal (and any
// misconfiguration worth logging); it is "" when the answer needs no comment.
//
// The tag decides when it exists, because the tag is what every OTHER daemon
// reads. Without it the fleet has no agreed hub, so the fallbacks are: a daemon
// that believes it is the hub claims, a lone daemon claims, and anything else
// defers rather than fight a neighbour for the record.
func DevDNSOwner(st *ipnstate.Status, selfID string, hubRole bool) (owns bool, reason string) {
	if owner := HubOwner(st); owner != "" {
		if owner == selfID {
			return true, ""
		}
		return false, fmt.Sprintf("%s owns it", owner)
	}
	if hubRole {
		return true, fmt.Sprintf("no node carries %s — tag this node so member hosts defer to it", HubTag)
	}
	if others := otherMembers(st, selfID); others > 0 {
		return false, fmt.Sprintf("no node carries %s and %d other ccmux host(s) are on the tailnet — tag the hub", HubTag, others)
	}
	return true, "" // alone on the tailnet: the record is this daemon's to hold
}

// otherMembers counts tag:ccmux nodes other than selfID.
func otherMembers(st *ipnstate.Status, selfID string) int {
	n := 0
	for _, node := range nodesFromStatus(st, CcmuxTag) {
		if node.ID != selfID {
			n++
		}
	}
	return n
}

// hubURLFromStatus is DiscoverHub's pure core: the base URL of the tag:ccmux-hub
// node other than selfID, or "".
func hubURLFromStatus(st *ipnstate.Status, selfID string) string {
	consider := func(ps *ipnstate.PeerStatus) string {
		if ps == nil || ps.DNSName == "" || !hasTag(peerTags(ps), HubTag) {
			return ""
		}
		if n := nodeOf(ps); n.ID != selfID {
			return "https://" + n.Addr
		}
		return ""
	}
	if url := consider(st.Self); url != "" {
		return url
	}
	for _, ps := range st.Peer {
		if url := consider(ps); url != "" {
			return url
		}
	}
	return ""
}

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
	ips := make([]string, 0, len(ps.TailscaleIPs))
	for _, ip := range ps.TailscaleIPs {
		ips = append(ips, ip.String())
	}
	return Node{ID: magicDNSLabel(ps.DNSName), Addr: addr, IPs: ips}
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
