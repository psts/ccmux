package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
)

// Identity aliases collapse the several names one human answers to into the
// single login the daemon keys on.
//
// The daemon learns who a caller is two ways. Over the tailnet it asks WhoIs and
// gets a verified login (an email). Over loopback WhoIs declines, so it falls
// back to whatever name the client declared: the Mac app sends NSFullUserName(),
// the web client sends a name typed into a browser prompt. One person therefore
// arrives as "Patric Sandelin" from the Mac and "patric@example.com" from their
// phone — and every comparison keyed on login, push suppression above all, treats
// them as two different people.
//
// An alias maps a self-declared name onto the verified login it belongs to. It
// applies only to the unverified fallback: a login WhoIs vouched for is already
// canonical and is never rewritten.

const settingIdentityAliases = "identity_aliases"

// ErrIncompleteAlias marks an alias row with an empty name or login, so the API
// can answer 400 rather than accept it and quietly store something else.
var ErrIncompleteAlias = errors.New("an identity alias needs both a name and a login")

// IdentityAliases returns the configured name → login map. The keys are
// lowercased on write, so callers must lowercase before looking one up (or use
// ResolveAlias, which does).
//
// A store-less Manager (tests construct one with manager.New(ctx, nil, nil)) has
// no settings at all, so it has no aliases — that is the honest answer, and it
// keeps this off the panic path of the HTTP handlers that resolve identity.
func (m *Manager) IdentityAliases() map[string]string {
	if m.store == nil {
		return map[string]string{}
	}
	raw := m.getSetting(settingIdentityAliases)
	if raw == "" {
		return map[string]string{}
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		// Loud, because the silent version is indistinguishable from "no aliases
		// configured" — which is exactly the state whose push notifications go to
		// the wrong place. See resolveIdentity.
		log.Printf("identity aliases: stored value is not valid JSON (%v); treating as empty, push suppression will not match self-declared names", err)
		return map[string]string{}
	}
	return out
}

// SetIdentityAliases persists the map, lowercasing the keys so a name's
// capitalisation can't decide whether suppression works. Values keep their case:
// a verified login is stored the way the identity provider spells it.
//
// A row with an empty side is rejected rather than dropped. Dropping it would
// answer 200 to a request whose effect differs from what was sent, and the one
// symptom would be push notifications continuing to arrive.
func (m *Manager) SetIdentityAliases(aliases map[string]string) error {
	clean := map[string]string{}
	for name, login := range aliases {
		name = strings.ToLower(strings.TrimSpace(name))
		login = strings.TrimSpace(login)
		if name == "" || login == "" {
			return fmt.Errorf("%w: %q → %q", ErrIncompleteAlias, name, login)
		}
		clean[name] = login
	}
	data, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	return m.store.SetSetting(settingIdentityAliases, string(data))
}

// ResolveAlias maps a self-declared name to its canonical login, or returns the
// name unchanged when no alias covers it. Matching ignores case and surrounding
// space, because these names are typed by humans into a prompt or read from a
// macOS account, and neither is a stable identifier.
func (m *Manager) ResolveAlias(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return name
	}
	if login, ok := m.IdentityAliases()[key]; ok {
		return login
	}
	return name
}
