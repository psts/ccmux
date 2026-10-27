package agent

import (
	"errors"
	"testing"
	"time"
)

func TestLatestCache(t *testing.T) {
	calls := make(chan struct{}, 4)
	answer, fail := "0.3.311", false
	c := &latestCache{fetch: func() (string, error) {
		defer func() { calls <- struct{}{} }()
		if fail {
			return "", errors.New("offline")
		}
		return answer, nil
	}}
	t0 := time.Unix(1_800_000_000, 0)
	if v := c.get(t0); v != "" {
		t.Errorf("nothing known before the first answer: %q", v)
	}
	<-calls
	waitFor(t, func() bool { return c.get(t0) == "0.3.311" })
	answer = "0.3.312"
	if v := c.get(t0.Add(time.Hour)); v != "0.3.311" {
		t.Errorf("within the window npm is not asked again: %q", v)
	}
	fail = true
	c.get(t0.Add(refreshEvery))
	<-calls
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return !c.inFlight })
	if v := c.get(t0.Add(refreshEvery)); v != "0.3.311" {
		t.Errorf("a failed ask keeps the last answer: %q", v)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never held")
}
