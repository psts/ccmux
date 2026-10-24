// Package repoowns keeps each repo's "owns" line: what that repo takes care
// of, as the bus tells every session in a window, so a session whose task
// needs work in another repo knows whom to hand it to. The line is a small
// model's summary of the repo's CLAUDE.md and README, written again only
// when those files change, unless a human has typed one (which wins).
package repoowns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// Store is the slice of the registry this needs.
type Store interface {
	RepoOwns(repoPath string) (store.RepoOwns, error)
	SetRepoOwnsAuto(repoPath, auto, hash string, now int64) error
}

// Completer asks the small model one question (llmproxy.Service.Complete).
type Completer func(ctx context.Context, prompt string, maxTokens int) (string, error)

const (
	// sourceCap bounds what is read of each file: the opening of a README
	// or CLAUDE.md says what the repo is; the rest is detail.
	sourceCap = 6000
	// maxLine bounds the stored line, whatever the model says.
	maxLine = 160
	// retryAfter keeps a failing model (no account, a refusal) from being
	// asked again on every session start.
	retryAfter = 10 * time.Minute
	// promptVersion is part of the hash: changing the prompt rewrites every
	// line on its next read.
	promptVersion = "owns-v1"
)

// sourceFiles are read in this order; a missing one is skipped.
var sourceFiles = []string{"CLAUDE.md", "README.md"}

// Service hands out lines and refreshes them in the background.
type Service struct {
	st  Store
	ask Completer
	now func() time.Time

	mu       sync.Mutex
	inflight map[string]bool
	failedAt map[string]time.Time // repo → when its last summary failed
	wg       sync.WaitGroup
}

func New(st Store, ask Completer) *Service {
	return &Service{st: st, ask: ask, now: time.Now, inflight: map[string]bool{}, failedAt: map[string]time.Time{}}
}

// Line is the repo's line as it stands, and starts a fresh summary in the
// background when the repo's files changed since the last one. It never
// waits on the model: a session starting now gets what is stored.
func (s *Service) Line(name, repoPath string) string {
	o, err := s.st.RepoOwns(repoPath)
	if err != nil {
		log.Printf("repoowns: read %s: %v", repoPath, err)
		return ""
	}
	if src, hash := Source(repoPath); src != "" && hash != o.AutoHash && s.claim(repoPath) {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.release(repoPath)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			if err := s.summarize(ctx, name, repoPath, src, hash); err != nil {
				log.Printf("repoowns: %s: %v (retry in %s)", repoPath, err, retryAfter)
				s.mu.Lock()
				s.failedAt[repoPath] = s.now()
				s.mu.Unlock()
			}
		}()
	}
	return o.Line()
}

// Pending reports whether a summary of repoPath is running now, so a
// caller waiting for its line knows whether one is coming.
func (s *Service) Pending(repoPath string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight[repoPath]
}

// Wait blocks until the background summaries started so far are done (tests).
func (s *Service) Wait() { s.wg.Wait() }

// claim marks a repo's summary as running, unless one already is or the
// last one failed too recently.
func (s *Service) claim(repoPath string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[repoPath] || s.now().Sub(s.failedAt[repoPath]) < retryAfter {
		return false
	}
	s.inflight[repoPath] = true
	return true
}

func (s *Service) release(repoPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, repoPath)
}

func (s *Service) summarize(ctx context.Context, name, repoPath, src, hash string) error {
	answer, err := s.ask(ctx, Prompt(name, src), 80)
	if err != nil {
		return err
	}
	line := Clean(answer)
	if line == "" {
		return errEmpty
	}
	return s.st.SetRepoOwnsAuto(repoPath, line, hash, s.now().UnixMilli())
}

var errEmpty = errors.New("the model answered nothing usable")

// Source is what a summary is made from (the repo's CLAUDE.md and README,
// each cut to sourceCap) and its hash. Both "" for a repo with neither.
func Source(repoPath string) (string, string) {
	var b strings.Builder
	for _, f := range sourceFiles {
		raw, err := os.ReadFile(filepath.Join(repoPath, f))
		if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		if len(raw) > sourceCap {
			raw = raw[:sourceCap]
		}
		b.WriteString("=== " + f + " ===\n" + string(raw) + "\n\n")
	}
	if b.Len() == 0 {
		return "", ""
	}
	sum := sha256.Sum256([]byte(promptVersion + "\x00" + b.String()))
	return b.String(), hex.EncodeToString(sum[:])
}

// Prompt asks for one line other agents can route work by.
func Prompt(name, src string) string {
	return "Other AI agents working in the same project need to know what the repository \"" + name +
		"\" is responsible for, so they hand it any work in its area instead of doing it themselves.\n" +
		"From its files below, reply with ONE line of at most 15 words: a comma-separated list of what this repo owns " +
		"(its areas, features and systems). No preamble, no quotes, no trailing period.\n\n" + src
}

// Clean keeps the first non-empty line of an answer, without wrapping
// quotes, a trailing period or a leading label, cut to maxLine.
func Clean(answer string) string {
	for _, l := range strings.Split(answer, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		l = strings.Trim(l, "\"'`")
		if i := strings.Index(l, ":"); i > 0 && i < 20 && strings.EqualFold(strings.TrimSpace(l[:i]), "owns") {
			l = strings.TrimSpace(l[i+1:])
		}
		l = strings.TrimSuffix(strings.Trim(l, "\"'`"), ".")
		if r := []rune(l); len(r) > maxLine {
			l = strings.TrimSpace(string(r[:maxLine])) + "…"
		}
		return l
	}
	return ""
}
