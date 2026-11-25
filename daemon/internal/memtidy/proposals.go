package memtidy

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"ccmux.dev/ccmuxd/internal/todos"
)

// The tidy-up's Claude may also leave ./proposals.json: problems that keep
// coming back, each with a fix it proposes, filed as to-do items on the
// repo's list for the person. It never changes CLAUDE.md, skills or
// settings itself.
const (
	proposalsFile = "proposals.json"
	maxProposals  = 3
	// proposalsMax is the most of the file that is read: three one-line
	// items are far less.
	proposalsMax = 16 << 10
	proposedBy   = "memory tidy-up"
)

type proposal struct {
	Text    string `json:"text"`
	Urgency string `json:"urgency"`
}

// readProposals reads and checks what Claude proposed in the work folder:
// at most maxProposals, each one line in the to-do list's rules and
// nothing that looks like a secret. A missing file is none; a malformed
// one is logged and is none too: it is never a reason to fail the night.
func readProposals(work, repo string, secrets []string) []proposal {
	b, err := readSmall(filepath.Join(work, proposalsFile), proposalsMax)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var in []proposal
	if err == nil {
		err = json.Unmarshal(b, &in)
	}
	if err != nil {
		log.Printf("memory tidy: %s: %s left out: %v", repo, proposalsFile, err)
		return nil
	}
	if len(in) > maxProposals {
		log.Printf("memory tidy: %s: %d proposals, the last %d dropped", repo, len(in), len(in)-maxProposals)
		in = in[:maxProposals]
	}
	var out []proposal
	for i, p := range in {
		p, why := checkProposal(p, secrets)
		if why != "" {
			log.Printf("memory tidy: %s: proposal %d dropped: %s", repo, i+1, why)
			continue
		}
		out = append(out, p)
	}
	return out
}

// checkProposal is a proposal as it is filed, or why it is not.
func checkProposal(p proposal, secrets []string) (proposal, string) {
	text, err := todos.CheckText(p.Text)
	if err != nil {
		return p, err.Error()
	}
	urgency, err := todos.CheckUrgency(p.Urgency)
	if err != nil {
		return p, err.Error()
	}
	if s := newSecret(nil, []byte(text), secrets); s != "" {
		return p, "it holds " + s
	}
	return proposal{Text: text, Urgency: urgency}, ""
}

func readSmall(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > max {
		return nil, fmt.Errorf("not a file of at most %d bytes", max)
	}
	b := make([]byte, fi.Size())
	_, err = f.ReadAt(b, 0)
	return b, err
}

// fileProposals puts the night's proposals on the repo's to-do list and
// says how many were filed. The list folds an item seen again into the
// one it has.
func (t *Tidier) fileProposals(repo string, ps []proposal) int {
	if t.Todo == nil {
		return 0
	}
	n := 0
	for _, p := range ps {
		if err := t.Todo(repo, p.Text, p.Urgency); err != nil {
			log.Printf("memory tidy: %s: a proposal could not be filed: %v", repo, err)
			continue
		}
		n++
	}
	return n
}
