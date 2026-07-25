package api

import "testing"

func TestIsHelloFrame(t *testing.T) {
	if !isHelloFrame([]byte(`{"t":"hello","attention":[]}`)) {
		t.Error("hello frame not recognized")
	}
	if isHelloFrame([]byte(`{"t":"attention","workspace":"w","pane":"p","state":"idle"}`)) {
		t.Error("attention frame misread as hello")
	}
	if isHelloFrame([]byte(`not json`)) {
		t.Error("garbage must not read as hello")
	}
}
