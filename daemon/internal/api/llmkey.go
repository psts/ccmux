package api

import (
	"errors"
	"log"
	"net/http"

	"ccmux.dev/ccmuxd/internal/llmproxy"
)

// llmAccountKey reveals one account's stored credential — the Claude
// setup-token or api key the settings editor otherwise only reports as
// "set". GET /v1/settings stays redacted because it answers anyone who can
// reach the daemon; this route answers only a caller the daemon can vouch
// for on its own authority: a WhoIs-verified tailnet login, or the machine's
// configured owner over loopback. The alias tier is deliberately NOT enough:
// it rewrites a self-declared ?user=, which a tagged node (a hub proxying to
// a member) could claim as easily as the person it belongs to.
//
// Unknown account and keyless account are both 404s with their own reason;
// an unreadable store is a 503, same as the settings read.
func (s *Server) llmAccountKey(w http.ResponseWriter, r *http.Request) {
	id := s.resolveIdentity(r)
	if !canRevealSecrets(id) {
		writeError(w, http.StatusForbidden,
			"revealing a stored key needs a verified tailnet login or this machine's owner on loopback")
		return
	}
	name := r.PathValue("name")
	key, err := s.llm.AccountKey(name)
	if err != nil {
		code := http.StatusServiceUnavailable
		if errors.Is(err, llmproxy.ErrUnknownAccount) || errors.Is(err, llmproxy.ErrNoStoredKey) {
			code = http.StatusNotFound
		}
		writeError(w, code, err.Error())
		return
	}
	// An audit line, never the value: who read which secret is worth a log
	// entry; the secret itself is not.
	log.Printf("llm: stored key of account %q revealed to %s (%s)", name, id.Login, id.Source)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "apiKey": key})
}

// canRevealSecrets is the identity bar for handing back a stored secret:
// verified by WhoIs, or the owner tier (which resolveIdentity already bounds
// to loopback). Vouched alone is too weak — see llmAccountKey.
func canRevealSecrets(id identity) bool {
	return id.Verified || id.Source == "owner"
}
