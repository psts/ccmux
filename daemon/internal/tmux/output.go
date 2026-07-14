// Package tmux implements a control-mode client for a dedicated tmux server.
// ccmuxd is the single tmux client; lenses never run tmux themselves.
package tmux

import "strconv"

// UnescapeOutput decodes the data field of a control-mode `%output` line back to
// raw pane bytes.
//
// tmux 3.6b escaping (verified empirically against the running server):
//   - C0 controls 0x00–0x1f and backslash 0x5c are written as `\ooo`, a
//     backslash followed by exactly three octal digits.
//   - Every other byte (0x20–0x7e except 0x5c, DEL 0x7f, and all high bytes
//     0x80–0xff including UTF-8) is written literally.
//
// A backslash therefore always introduces a 3-digit octal escape; tmux never
// emits a lone backslash. We still degrade gracefully if one appears.
func UnescapeOutput(s []byte) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s)+1 && isOctal(at(s, i+1)) && isOctal(at(s, i+2)) && isOctal(at(s, i+3)) {
			v := (int(s[i+1]-'0') << 6) | (int(s[i+2]-'0') << 3) | int(s[i+3]-'0')
			out = append(out, byte(v))
			i += 3
			continue
		}
		out = append(out, s[i])
	}
	return out
}

func isOctal(b byte) bool { return b >= '0' && b <= '7' }

// at returns the byte at i, or 0xff (a non-octal sentinel) if out of range.
func at(s []byte, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0xff
}

// HexKeys renders raw bytes as the space-separated lowercase hex arguments that
// `tmux send-keys -H` expects, e.g. []byte{0x1b,'[','A'} -> "1b 5b 41". This is
// how lens keystrokes reach a pane: in-band on the control connection, no
// fork/exec, honoring the "no process spawning in hot paths" rule.
func HexKeys(b []byte) []string {
	args := make([]string, len(b))
	for i, c := range b {
		args[i] = strconv.FormatInt(int64(c), 16)
	}
	return args
}
