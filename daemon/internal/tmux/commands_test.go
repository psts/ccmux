package tmux

import (
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
