package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// opencode's TUI serves its HTTP API on --port; that is how a prompt reaches
// a running instance from outside the terminal. --prompt on the command line
// only prefills the input (seen live 2026-09-05: the session was created and
// nothing was sent), so the daemon types the launch line and then pushes the
// first message through the API. The same path carries peer messages later.

var portFlag = regexp.MustCompile(`--port (\d+)`)

// FreePort asks the kernel for an unused loopback port for a new instance.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// OpencodePort reads the --port an instance was started with back out of its
// persisted startup command; 0 when it has none (not an opencode instance).
func OpencodePort(startupCommand string) int {
	m := portFlag.FindStringSubmatch(startupCommand)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// PushPrompt waits for the instance's server to answer on port, then appends
// text to the TUI's prompt and submits it. Bounded by ctx; a TUI that never
// comes up is an error the caller logs, not a hang.
func PushPrompt(ctx context.Context, port int, text string) error {
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: 5 * time.Second}
	if err := waitUp(ctx, client, base+"/session"); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	if err := post(ctx, client, base+"/tui/append-prompt", body); err != nil {
		return fmt.Errorf("append-prompt: %w", err)
	}
	if err := post(ctx, client, base+"/tui/submit-prompt", []byte("{}")); err != nil {
		return fmt.Errorf("submit-prompt: %w", err)
	}
	return nil
}

func waitUp(ctx context.Context, client *http.Client, url string) error {
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("opencode server on %s did not come up: %w", url, ctx.Err())
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func post(ctx context.Context, client *http.Client, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s → %d", url, resp.StatusCode)
	}
	return nil
}
