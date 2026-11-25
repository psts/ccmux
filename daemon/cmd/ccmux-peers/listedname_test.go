package main

import "testing"

// A repo's main session is listed with the plain name it also answers to,
// so a peer reading the list knows a message to that name reaches it.
func TestAListedPeerShowsThePlainNameItAnswersTo(t *testing.T) {
	for want, p := range map[string]listEntry{
		"sommelia-s-3aad55a4 (answers to sommelia)": {ID: "ab12cd34", Name: "sommelia-s-3aad55a4", AnswersTo: "sommelia"},
		"backend":   {ID: "ab12cd34", Name: "backend"},
		"(unnamed)": {ID: "ab12cd34"},
	} {
		if got := listedName(p); got != want {
			t.Errorf("listedName(%+v) = %q, want %q", p, got, want)
		}
	}
}
