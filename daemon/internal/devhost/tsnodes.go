package devhost

import (
	"log"
	"net/http"
	"os"
	"path/filepath"

	"tailscale.com/tsnet"
)

// ts.net fallback mode: with no dev domain configured, each hostname becomes
// its own tsnet node — https://<name>.<tailnet>.ts.net with a Tailscale-issued
// cert, zero DNS configuration anywhere. The auth key setting registers nodes
// silently; without it the join URL lands in the daemon log.

// nodeHandle is a running node's shutdown handle (*tsnet.Server in production;
// tests inject fakes so reconcile logic never dials the Tailscale control plane).
type nodeHandle interface{ Close() error }

// fallbackNode is one running per-hostname tsnet node.
type fallbackNode struct {
	handle  nodeHandle
	authKey string // the key it was registered with, to restart on key change
}

// reconcileNodesLocked diffs desired hostnames against running nodes: missing
// ones start, orphaned ones stop (their node identity is deleted too, so the
// tailnet doesn't accumulate dead devices). A key change restarts nodes — that
// unsticks one that sat at a login URL because no key was set. Ports are
// resolved per request through the shared table, so a port-only change needs
// no node restart.
func (s *Server) reconcileNodesLocked(names map[string]int) {
	authKey := s.state.TailscaleAuthKey()
	for name, node := range s.nodes {
		_, want := names[name]
		if want && node.authKey == authKey {
			continue
		}
		node.handle.Close()
		delete(s.nodes, name)
		if !want {
			s.removeNodeState(name)
		}
	}
	for name := range names {
		if _, running := s.nodes[name]; running {
			continue
		}
		s.nodes[name] = &fallbackNode{handle: s.newNode(name, authKey), authKey: authKey}
	}
}

// startNode brings up one fallback node in the background: node state persists
// under dataDir/tsnet-hosts/<name> so re-registration (and the auth key) is
// only needed once per hostname.
func (s *Server) startNode(name, authKey string) nodeHandle {
	ts := &tsnet.Server{
		Hostname: name,
		Dir:      filepath.Join(s.dataDir, "tsnet-hosts", name),
		AuthKey:  authKey,
		UserLogf: log.Printf, // surfaces the join URL when no auth key is set
	}
	handler := s.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // node serves only its own name; nothing falls through
	}))
	go func() {
		ln, err := ts.ListenTLS("tcp", ":443")
		if err != nil {
			log.Printf("devhost: node %s: %v", name, err)
			return
		}
		log.Printf("devhost: node %s up (https://%s.%s)", name, name, s.tsSuffix)
		if err := http.Serve(ln, handler); err != nil && err != http.ErrServerClosed {
			log.Printf("devhost: node %s stopped: %v", name, err)
		}
	}()
	return ts
}

// stopNodesLocked shuts down every fallback node (entering custom-domain mode).
// Node state dirs are kept — flipping back re-adopts the same node identities.
func (s *Server) stopNodesLocked() {
	for name, node := range s.nodes {
		node.handle.Close()
		delete(s.nodes, name)
	}
}

// removeNodeState deletes a hostname's persisted node identity.
func (s *Server) removeNodeState(name string) {
	_ = os.RemoveAll(filepath.Join(s.dataDir, "tsnet-hosts", name))
}
