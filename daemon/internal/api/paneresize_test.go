package api

import (
	"net/http"
	"testing"
	"time"
)

// TestAPI_PaneResizeOverREST: the attention board sizes a passive tile's pane
// to the tile with no attach socket. It is the same resize a lens sends: the
// pane takes the size, an attached lens hears it as pane-size (so its "take
// over" shows), and the snapshot the tile draws next reports it. A size tmux
// will not take, a body that is not JSON and an unknown pane are refused.
func TestAPI_PaneResizeOverREST(t *testing.T) {
	_, base := floodStack(t, "ccmux-paneresize-itest")

	ws := createWS(t, base)
	pane0 := ws.Panes[0].ID
	lens := attachAndHello(t, base, ws.ID)
	defer lens.Close()

	resp := postJSON(t, base+"/v1/panes/"+pane0+"/resize", "", map[string]int{"cols": 81, "rows": 22})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resize = %d, want 200", resp.StatusCode)
	}

	lens.SetReadDeadline(time.Now().Add(5 * time.Second))
	heard := false
	for i := 0; i < 200 && !heard; i++ {
		var m wsMsg
		if err := lens.ReadJSON(&m); err != nil {
			t.Fatalf("read: %v", err)
		}
		heard = m.T == "pane-size" && m.Pane == pane0 && m.Cols == 81 && m.Rows == 22
	}
	if !heard {
		t.Fatal("the attached lens never heard the pane-size 81x22")
	}

	_, snap := getJSON(t, base+"/v1/panes/"+pane0+"/snapshot?plain=1")
	if snap["cols"] != float64(81) || snap["rows"] != float64(22) {
		t.Fatalf("snapshot size = %vx%v, want 81x22", snap["cols"], snap["rows"])
	}

	for name, c := range map[string]struct {
		pane string
		body any
		want int
	}{
		"zero size":    {pane0, map[string]int{"cols": 0, "rows": 22}, http.StatusBadRequest},
		"too big":      {pane0, map[string]int{"cols": 20000, "rows": 22}, http.StatusBadRequest},
		"not json":     {pane0, "81x22", http.StatusBadRequest},
		"unknown pane": {"nope", map[string]int{"cols": 81, "rows": 22}, http.StatusNotFound},
	} {
		if got := postJSON(t, base+"/v1/panes/"+c.pane+"/resize", "", c.body).StatusCode; got != c.want {
			t.Errorf("%s: resize = %d, want %d", name, got, c.want)
		}
	}
}
