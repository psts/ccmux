package model

import "testing"

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"/Users/pat/Work/Coding/ccmux": "ccmux",
		"/Users/pat/My Project":        "my-project",
		"/tmp/foo_bar.baz/":            "foo-bar-baz",
		"/tmp/---weird!!!name---":      "weird-name",
		"":                             "repo",
		"/":                            "repo",
		"/a/UPPER":                     "upper",
		"/x/averylongrepositorynamethatexceedslimit": "averylongrepositorynamet",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionName(t *testing.T) {
	got := SessionName("ccmux", "3f9a2b1c-dead-beef-0000-111122223333")
	want := "ccmux-ccmux-3f9a2b1c"
	if got != want {
		t.Fatalf("SessionName = %q, want %q", got, want)
	}
	// Short ids shouldn't panic or over-slice.
	if got := SessionName("x", "ab"); got != "ccmux-x-ab" {
		t.Fatalf("short id: got %q", got)
	}
}
