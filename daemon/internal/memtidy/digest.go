package memtidy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Sizes of the digest the tidy-up reads instead of the raw transcripts,
// which are mostly tool output and run to megabytes.
const (
	msgCap   = 1500   // one message
	convHead = 10_000 // of a long conversation: its start...
	convTail = 30_000 // ...and its end, where the lessons usually are
)

// repoBudget is every conversation of one repo, one night (a variable so a
// test can make a night overflow).
var repoBudget = 400_000

// line is the part of a transcript line the digest reads.
type line struct {
	Type    string `json:"type"`
	IsMeta  bool   `json:"isMeta"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// digest is a conversation as plain text: what the person and Claude
// wrote, and a short line per tool call. Tool results are left out.
func digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	lines, parsed := 0, 0
	for sc.Scan() {
		lines++
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		parsed++
		if !l.IsMeta && (l.Type == "user" || l.Type == "assistant") {
			writeMessage(&b, l)
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	// Not one line read: the transcript's format is not what this knows
	// (a new Claude Code), which must show as unreadable, not as empty.
	if lines > 0 && parsed == 0 {
		return "", fmt.Errorf("none of its %d lines reads as a transcript line", lines)
	}
	return headTail(b.String(), convHead, convTail), nil
}

func writeMessage(b *strings.Builder, l line) {
	who := "Person"
	if l.Type == "assistant" {
		who = "Claude"
	}
	var s string
	if json.Unmarshal(l.Message.Content, &s) == nil {
		writeText(b, who, s)
		return
	}
	var blocks []block
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return
	}
	for _, bl := range blocks {
		switch bl.Type {
		case "text":
			writeText(b, who, bl.Text)
		case "tool_use":
			fmt.Fprintf(b, "[tool %s %s]\n", bl.Name, clip(string(bl.Input), 160))
		}
	}
}

func writeText(b *strings.Builder, who, s string) {
	if s = strings.TrimSpace(s); s != "" {
		fmt.Fprintf(b, "%s: %s\n\n", who, clip(s, msgCap))
	}
}

// clip cuts s to n bytes on a rune boundary, saying it did.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n] + " [...]"
}

func utf8Start(c byte) bool { return c&0xC0 != 0x80 }

// headTail keeps the start and the end of a long text.
func headTail(s string, head, tail int) string {
	if len(s) <= head+tail {
		return s
	}
	h, t := head, len(s)-tail
	for h > 0 && !utf8Start(s[h]) {
		h--
	}
	for t < len(s) && !utf8Start(s[t]) {
		t++
	}
	return s[:h] + "\n[... the middle of this conversation is left out ...]\n\n" + s[t:]
}
