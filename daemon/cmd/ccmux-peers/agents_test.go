package main

import (
	"strings"
	"testing"
)

// The rendering lives in internal/buscontext and is tested there; this pins
// that the shim carries both lists and says nothing outside a window.
func TestWindowParagraphAndSectionWithoutADaemon(t *testing.T) {
	a := &app{}
	if a.windowParagraph() != "" || a.windowSection() != "" {
		t.Fatal("no daemon: nothing appended")
	}
	if !strings.Contains(strings.ToLower(serverInstructions), "peer") {
		t.Fatal("frozen instructions missing")
	}
}
