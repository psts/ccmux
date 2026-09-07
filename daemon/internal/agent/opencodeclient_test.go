package agent

import (
	"ccmux.dev/ccmuxd/internal/agent/agenttest"
	"context"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func client(f *agenttest.FakeOpencode) *Opencode {
	return &Opencode{Base: f.Server.URL, HTTP: &http.Client{Timeout: 2 * time.Second}}
}

func TestOpencodeClientRoundTrips(t *testing.T) {
	f := agenttest.NewFakeOpencode()
	defer f.Server.Close()
	c := client(f)
	ctx := context.Background()
	if !c.Up(ctx) {
		t.Fatal("up")
	}
	sessions, err := c.Sessions(ctx)
	if err != nil || len(sessions) != 2 || sessions[0].ID != "ses_1" || sessions[0].Directory != "/inst" {
		t.Fatalf("sessions newest first: %v %v", sessions, err)
	}
	turns, err := c.Messages(ctx, "ses_1")
	if err != nil || len(turns) != 2 {
		t.Fatalf("messages: %v %v", turns, err)
	}
	if err := c.Prompt(ctx, "ses_1", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := c.Abort(ctx, "ses_1"); err != nil {
		t.Fatal(err)
	}
	perms, err := c.Permissions(ctx)
	if err != nil || len(perms) != 1 || perms[0].Permission != "bash" || perms[0].Patterns[0] != "ls *" {
		t.Fatalf("permissions: %+v %v", perms, err)
	}
	if err := c.ReplyPermission(ctx, "per_1", "once"); err != nil {
		t.Fatal(err)
	}
	if err := c.ReplyPermission(ctx, "per_1", "maybe"); err == nil {
		t.Error("an unknown reply must be refused before it reaches opencode")
	}
	prompts, aborts, replies := f.Recorded()
	if len(prompts) != 1 || prompts[0] != "ses_1:hello" || aborts != 1 || replies["per_1"] != "once" {
		t.Errorf("recorded: prompts %v aborts %d replies %v", prompts, aborts, replies)
	}
}

func TestOpencodeEventsStreamUntilCancelled(t *testing.T) {
	f := agenttest.NewFakeOpencode()
	defer f.Server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan OpencodeEvent, 8)
	done := make(chan error, 1)
	go func() { done <- client(f).Events(ctx, func(ev OpencodeEvent) { got <- ev }) }()
	f.Events <- `{"type":"session.idle","properties":{"sessionID":"ses_1"}}`
	f.Events <- `not json at all`
	f.Events <- `{"type":"message.part.delta","properties":{"sessionID":"ses_1","messageID":"m","partID":"p","field":"text","delta":"hi"}}`
	var types []string
	for len(types) < 3 {
		select {
		case ev := <-got:
			types = append(types, ev.Type)
		case <-time.After(3 * time.Second):
			t.Fatalf("events so far: %v", types)
		}
	}
	if strings.Join(types, ",") != "server.connected,session.idle,message.part.delta" {
		t.Fatalf("types = %v (garbage skipped, order kept)", types)
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("after cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Events did not return after cancel")
	}
}

func TestOfflineSessionsFiltersByDirectory(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mine, err := OfflineSessions(ctx, "/nowhere/at/all")
	if err != nil || len(mine) != 0 {
		t.Fatalf("a folder with no sessions lists none: %v %v", mine, err)
	}
}
