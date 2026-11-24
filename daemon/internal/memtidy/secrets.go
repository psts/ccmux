package memtidy

import (
	"bytes"
	"regexp"
)

// secretShapes are what a key or token looks like when a conversation
// printed one. The tidy-up reads conversations, so it must never carry one
// into memory, which every later session loads.
var secretShapes = []struct {
	name string
	re   *regexp.Regexp
}{
	{"an Anthropic key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)},
	{"an API key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{32,}`)},
	{"a GitHub token", regexp.MustCompile(`\b(ghp|gho|ghs|ghu|ghr)_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}`)},
	{"a Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"an AWS key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"a private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"a bearer token", regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{24,}`)},
	{"a JWT", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
}

// newSecret names a secret new has that old did not: a known shape, or one
// of the literal values given (the daemon's API token). "" when none.
func newSecret(old, new []byte, literals []string) string {
	for _, lit := range literals {
		if len(lit) >= 8 && bytes.Contains(new, []byte(lit)) && !bytes.Contains(old, []byte(lit)) {
			return "a ccmux token"
		}
	}
	for _, s := range secretShapes {
		for _, m := range s.re.FindAll(new, -1) {
			if !bytes.Contains(old, m) {
				return s.name
			}
		}
	}
	return ""
}
