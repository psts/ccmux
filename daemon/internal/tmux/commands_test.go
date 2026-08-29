package tmux

import (
	"errors"
	"strings"
	"testing"
)

// The numbers here are not a guess. Against a live tmux 3.4 server, a
// `send-keys -H` carrying 9990 hex arguments parsed and 9995 answered
// "%error ... parse error: yacc stack overflow" with zero bytes delivered.
// maxKeysPerCommand must stay clear of that, and every chunk must stay clear
// of it too — a chunk is what actually reaches the parser.
const observedYaccLimit = 9990

func TestSendKeysCommands_ChunksBelowTheParserLimit(t *testing.T) {
	// 19 kB: the paste size that failed in full before chunking.
	data := make([]byte, 19*1024)
	cmds := sendKeysCommands("%3", data)
	if len(cmds) < 2 {
		t.Fatalf("19 kB should split, got %d command(s)", len(cmds))
	}
	for i, args := range cmds {
		if len(args) > observedYaccLimit {
			t.Fatalf("chunk %d has %d args, over the observed tmux limit of %d",
				i, len(args), observedYaccLimit)
		}
	}
}

func TestSendKeysCommands_PreservesEveryByteInOrder(t *testing.T) {
	data := make([]byte, maxKeysPerCommand*2+7)
	for i := range data {
		data[i] = byte(i) // wraps; exercises the full 0x00-0xff range
	}
	var got []string
	for _, args := range sendKeysCommands("%3", data) {
		if len(args) < 5 {
			t.Fatalf("chunk with no keys: %v", args)
		}
		if prefix := strings.Join(args[:4], " "); prefix != "send-keys -H -t %3" {
			t.Fatalf("bad chunk prefix %q", prefix)
		}
		got = append(got, args[4:]...)
	}
	want := HexKeys(data)
	if len(got) != len(want) {
		t.Fatalf("byte count changed: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d reordered: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestSendKeysCommands_SingleKeystrokeStaysOneCommand(t *testing.T) {
	// The hot path: one keypress must not gain a round trip.
	cmds := sendKeysCommands("%3", []byte{0x0d})
	if len(cmds) != 1 {
		t.Fatalf("one byte should be one command, got %d", len(cmds))
	}
	if want := []string{"send-keys", "-H", "-t", "%3", "d"}; strings.Join(cmds[0], " ") != strings.Join(want, " ") {
		t.Fatalf("got %v want %v", cmds[0], want)
	}
}

func TestSendKeysCommands_ExactChunkSizeDoesNotEmitEmptyTail(t *testing.T) {
	cmds := sendKeysCommands("%3", make([]byte, maxKeysPerCommand))
	if len(cmds) != 1 {
		t.Fatalf("an exact chunk should be one command, got %d", len(cmds))
	}
}

func TestSendKeysCommands_EmptyDataSendsNothing(t *testing.T) {
	if cmds := sendKeysCommands("%3", nil); len(cmds) != 0 {
		t.Fatalf("empty data should cost no round trip, got %v", cmds)
	}
}

// TestFormatCommand_RefusesNewlineInjection pins the fix for a real, verified
// injection. Control mode is line-based, so a newline inside an argument ends
// the command and the remainder runs as the next one. Quoting does not help:
// the framing happens before tmux parses quotes.
//
// Demonstrated against tmux 3.4 before the fix — a set-option whose value was
// "a\nrename-session -t it PWNED" renamed the session, single quotes and all.
// Reachable because SpawnWindow puts cwd and each env value straight into args
// and cwd is a user-chosen repo path arriving over HTTP. (A dev command does
// not reach here; it goes out as hex keystrokes.)
func TestFormatCommand_RefusesNewlineInjection(t *testing.T) {
	payload := "a\nrename-session -t it PWNED"
	line, err := formatCommand([]string{"set-option", "-t", "it", "@x", payload})
	if err == nil {
		t.Fatalf("formatCommand accepted a newline and produced %q — that is a "+
			"second tmux command, not an argument", line)
	}
	if line != "" {
		t.Errorf("a rejected command must render nothing, got %q", line)
	}

	// A carriage return frames the same way and must be refused too.
	if _, err := formatCommand([]string{"set-option", "@x", "a\rkill-server"}); err == nil {
		t.Error("formatCommand accepted a carriage return")
	}

	// #( is tmux's run-shell format. new-window expands its -c value when the
	// command runs, so this executes /bin/sh AFTER parsing — single quotes are
	// no defence. Confirmed on tmux 3.4: a cwd of "<dir>/#(id>/tmp/x)" created
	// the window normally and wrote the file, with no error anywhere.
	if _, err := formatCommand([]string{"new-window", "-c", "/repos/#(id>/tmp/pwned)"}); err == nil {
		t.Error("formatCommand accepted #( — that is a shell command on expansion")
	}

	// The refusal must not spread to arguments that merely need quoting —
	// paths with spaces and format strings are the everyday case.
	for _, ok := range [][]string{
		{"new-window", "-c", "/home/me/My Projects/app"},
		{"list-windows", "-F", "#{window_id}|#{pane_id}"},
		{"refresh-client", "-B", "ccmux-title:%*:#{pane_title}"},
		{"display-message", "-p", "#{cursor_x} #{cursor_y}"},
		{"new-window", "-c", "/home/me/c#sharp"}, // a bare # is not #(

		{"set-option", "-t", "it", "@ccmux_workspace_id", "83deb76b-9d03"},
		{"new-window", "-e", "PATH=/usr/bin:/bin"},
	} {
		if _, err := formatCommand(ok); err != nil {
			t.Errorf("formatCommand(%q) refused a legitimate command: %v", ok, err)
		}
	}
}

// TestSendChunks_ReportsWhatLanded is the producer side of PartialSendError.
// Before this, the type was only ever constructed by hand in a consumer test,
// so producer and consumer were checked against each other's assumptions and
// never against the code between them. Reverting the typed error to a
// fmt.Errorf left the whole suite green while errors.As stopped matching and
// the user-facing banner silently stopped appearing.
func TestSendChunks_ReportsWhatLanded(t *testing.T) {
	data := make([]byte, maxKeysPerCommand*3) // exactly three chunks
	failOn := 2                               // second chunk fails: one landed
	calls := 0
	boom := errors.New("connection closed")

	err := sendChunks("%3", data, func(args []string) error {
		calls++
		if calls == failOn {
			return boom
		}
		return nil
	})

	var partial *PartialSendError
	if !errors.As(err, &partial) {
		t.Fatalf("sendChunks returned %T (%v), want a *PartialSendError — the "+
			"lens recovers its byte counts with errors.As", err, err)
	}
	if partial.Sent != maxKeysPerCommand {
		t.Errorf("Sent = %d, want %d (one whole chunk landed before the failure)",
			partial.Sent, maxKeysPerCommand)
	}
	if partial.Total != len(data) {
		t.Errorf("Total = %d, want %d", partial.Total, len(data))
	}
	if partial.Pane != "%3" {
		t.Errorf("Pane = %q, want %%3", partial.Pane)
	}
	if !errors.Is(err, boom) {
		t.Error("the underlying cause must stay unwrappable")
	}
	if calls != failOn {
		t.Errorf("issued %d chunks, want %d — it must stop at the first failure", calls, failOn)
	}
}

// TestSendChunks_FirstChunkFailingReportsZero pins the boundary the lens keys
// on: Sent == 0 means nothing arrived, which now gets its own wording because
// at a password prompt it is indistinguishable from success.
func TestSendChunks_FirstChunkFailingReportsZero(t *testing.T) {
	err := sendChunks("%1", make([]byte, maxKeysPerCommand*2), func([]string) error {
		return errors.New("closed")
	})
	var partial *PartialSendError
	if !errors.As(err, &partial) || partial.Sent != 0 {
		t.Fatalf("got %v, want a PartialSendError with Sent 0", err)
	}
}

// TestSendChunks_SuccessAndEmpty keep the happy paths honest: a send that
// works reports no error, and empty data issues nothing at all.
func TestSendChunks_SuccessAndEmpty(t *testing.T) {
	calls := 0
	if err := sendChunks("%1", make([]byte, maxKeysPerCommand+1), func([]string) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("a fully delivered send must not error: %v", err)
	}
	if calls != 2 {
		t.Errorf("issued %d chunks for maxKeysPerCommand+1 bytes, want 2", calls)
	}

	calls = 0
	if err := sendChunks("%1", nil, func([]string) error { calls++; return nil }); err != nil || calls != 0 {
		t.Errorf("empty data: err=%v calls=%d, want nil and 0 — an empty send-keys "+
			"is a wasted round trip", err, calls)
	}
}
