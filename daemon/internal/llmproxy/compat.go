package llmproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// maxCompatBody bounds how much request we are willing to buffer for the
// rewrite. A Claude Code request with full tool definitions runs to a few
// hundred KB; anything past this cap is forwarded untouched rather than held
// in memory.
const maxCompatBody = 16 << 20

// needsSystemTurnCompat reports whether requests to this account must have
// mid-conversation system turns downgraded. Claude Code emits messages with
// role "system" inside the messages array (its system-turn mechanism);
// Anthropic's API accepts them, but Anthropic-compatible servers translate
// them literally and strict chat templates then refuse a system message that
// is not first (verified against Ollama 0.32.15 + Qwen). Only the real
// Anthropic host keeps them intact.
func needsSystemTurnCompat(a Account) bool {
	return !strings.Contains(a.BaseURL, "api.anthropic.com")
}

// rewriteRequest applies the account's request transforms — system-turn
// downgrade for non-Anthropic upstreams, and model aliasing — buffering the
// body once for both. Anything that stops the rewrite (too big, unreadable,
// not the JSON shape it expects) forwards the original bytes untouched: the
// upstream's parser owns malformed requests, not us. The path check covers
// /v1/messages and its sub-resources (count_tokens carries a model too).
func rewriteRequest(r *http.Request, a Account) {
	turns := needsSystemTurnCompat(a)
	if r.Body == nil || r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/v1/messages") {
		return
	}
	if !turns && len(a.ModelAliases) == 0 {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCompatBody+1))
	r.Body.Close()
	if err != nil || len(body) > maxCompatBody {
		restoreBody(r, body)
		return
	}
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		restoreBody(r, body)
		return
	}
	changed := aliasModel(req, a.ModelAliases)
	if turns {
		changed = downgradeSystemTurns(req) || changed
	}
	if !changed {
		restoreBody(r, body)
		return
	}
	if rewritten, err := json.Marshal(req); err == nil {
		restoreBody(r, rewritten)
	} else {
		restoreBody(r, body)
	}
}

// downgradeSystemTurns flips messages[].role "system" to "user" in place,
// reporting whether anything changed.
func downgradeSystemTurns(req map[string]json.RawMessage) bool {
	if req["messages"] == nil {
		return false
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(req["messages"], &msgs) != nil {
		return false
	}
	changed := false
	for _, m := range msgs {
		var role string
		if json.Unmarshal(m["role"], &role) == nil && role == "system" {
			m["role"] = json.RawMessage(`"user"`)
			changed = true
		}
	}
	if !changed {
		return false
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		return false
	}
	req["messages"] = b
	return true
}

// aliasModel rewrites the request's model through the account's alias list —
// what lets a local upstream answer for model names it has never heard of,
// like the haiku names Claude Code's background calls hardwire. First match
// wins; a From ending in '*' matches by prefix.
func aliasModel(req map[string]json.RawMessage, aliases []ModelAlias) bool {
	if len(aliases) == 0 || req["model"] == nil {
		return false
	}
	var model string
	if json.Unmarshal(req["model"], &model) != nil {
		return false
	}
	for _, al := range aliases {
		if !aliasMatches(al.From, model) || al.To == model {
			continue
		}
		b, err := json.Marshal(al.To)
		if err != nil {
			return false
		}
		req["model"] = b
		return true
	}
	return false
}

func aliasMatches(pattern, model string) bool {
	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(model, p)
	}
	return pattern == model
}

func restoreBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Del("Content-Length")
}
