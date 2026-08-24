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

// downgradeSystemTurns rewrites r's body so messages[].role "system" becomes
// "user", buffering the body to do it. Anything that stops the rewrite — too
// big, unreadable, not the JSON shape it expects — forwards the original
// bytes untouched: the upstream's parser owns malformed requests, not us.
func downgradeSystemTurns(r *http.Request) {
	if r.Body == nil || r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v1/messages") {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCompatBody+1))
	r.Body.Close()
	if err != nil || len(body) > maxCompatBody {
		restoreBody(r, body)
		return
	}
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil || req["messages"] == nil {
		restoreBody(r, body)
		return
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(req["messages"], &msgs) != nil {
		restoreBody(r, body)
		return
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
		restoreBody(r, body)
		return
	}
	if rewritten, err := marshalRewrite(req, msgs); err == nil {
		restoreBody(r, rewritten)
	} else {
		restoreBody(r, body)
	}
}

func marshalRewrite(req map[string]json.RawMessage, msgs []map[string]json.RawMessage) ([]byte, error) {
	m, err := json.Marshal(msgs)
	if err != nil {
		return nil, err
	}
	req["messages"] = m
	return json.Marshal(req)
}

func restoreBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Del("Content-Length")
}
