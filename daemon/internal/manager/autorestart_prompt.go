package manager

import (
	"strings"
	"unicode"
)

// promptEmpty reads a pane's screen (CaptureScreen, escapes kept) and says
// whether Claude's input box is on screen and holds nothing the human typed.
// The box is one "❯" line with a horizontal rule right above and below it.
// Claude draws its placeholder ("Try ...") dim, so dim text counts as empty
// and anything else does not. Every unsure reading is false: a restart typed
// over a draft would lose it, while a skipped pane just waits a tick.
func promptEmpty(screen []byte) bool {
	rows := strings.Split(string(screen), "\r\n")
	for i := len(rows) - 2; i >= 1; i-- {
		if !isRule(rows[i-1]) || !isRule(rows[i+1]) {
			continue
		}
		if !strings.HasPrefix(stripSGR(rows[i], false), "❯") {
			return false
		}
		typed := strings.TrimPrefix(stripSGR(rows[i], true), "❯")
		return strings.TrimFunc(typed, unicode.IsSpace) == ""
	}
	return false
}

// isRule is a line made only of box-drawing horizontals, ignoring colour.
func isRule(row string) bool {
	s := strings.TrimSpace(stripSGR(row, false))
	return len(s) > 0 && strings.Trim(s, "─") == ""
}

// stripSGR removes escape sequences from a captured row. dropDim also drops
// the text drawn while SGR 2 (dim) is on, which is how Claude draws its
// placeholder.
func stripSGR(row string, dropDim bool) string {
	var b strings.Builder
	dim := false
	for i := 0; i < len(row); i++ {
		if row[i] != 0x1b {
			if !(dropDim && dim) {
				b.WriteByte(row[i])
			}
			continue
		}
		end := csiEnd(row, i)
		if end > i && row[end] == 'm' {
			dim = sgrDim(row[i+2:end], dim)
		}
		i = end
	}
	return b.String()
}

// csiEnd returns the index of the final byte of the escape at i, or i when it
// is not a CSI sequence (a lone ESC is skipped).
func csiEnd(row string, i int) int {
	if i+1 >= len(row) || row[i+1] != '[' {
		return i
	}
	for j := i + 2; j < len(row); j++ {
		if row[j] >= 0x40 && row[j] <= 0x7e {
			return j
		}
	}
	return len(row) - 1
}

// sgrDim applies one SGR parameter list to the dim state: 2 sets it, 0, 22
// and an empty list clear it.
func sgrDim(params string, dim bool) bool {
	if params == "" {
		return false
	}
	for _, p := range strings.Split(params, ";") {
		switch p {
		case "2":
			dim = true
		case "0", "22":
			dim = false
		}
	}
	return dim
}
