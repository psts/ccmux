package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestHubInfo pins the lens-retargeting contract: GET /v1/hub answers 200 with
// the discovered hub's base URL, and "" (not 404) when there is nothing to
// retarget to — the wiring-absent and wired-but-empty cases must look the same
// to a lens.
func TestHubInfo(t *testing.T) {
	cases := []struct {
		name string
		fn   func() string
		want string
	}{
		{"unwired (hub node / no tsnet)", nil, ""},
		{"wired, no hub found yet", func() string { return "" }, ""},
		{"wired, hub discovered", func() string { return "https://hub.tail0.ts.net" }, "https://hub.tail0.ts.net"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{hubURLFn: tc.fn}
			rec := httptest.NewRecorder()
			s.hubInfo(rec, httptest.NewRequest("GET", "/v1/hub", nil))

			if rec.Code != 200 {
				t.Fatalf("hubInfo = %d, want 200", rec.Code)
			}
			var got struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v (%s)", err, rec.Body)
			}
			if got.URL != tc.want {
				t.Errorf("url = %q, want %q", got.URL, tc.want)
			}
		})
	}
}
