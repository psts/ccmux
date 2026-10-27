package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

// SDKVersions is the Claude Agent SDK the claude agents run on: pinned in
// this daemon's sidecar (claudeserve/package.json), installed under the
// agents root (it lands at the next claude start after a pin moves), and
// the newest on npm ("" until known, or when npm cannot be reached). The
// lenses say "agents on X, latest Y" from it, the way a pane tab says its
// Claude Code is behind.
type SDKVersions struct {
	Pinned    string `json:"pinned"`
	Installed string `json:"installed"`
	Latest    string `json:"latest"`
}

// SDKVersions reads the pinned and installed versions and the cached
// newest one, starting a refresh of that in the background when it is
// stale. It never waits on npm.
func (s *Store) SDKVersions() SDKVersions {
	pinned, _ := pinnedSDKVersion()
	return SDKVersions{
		Pinned:    pinned,
		Installed: installedSDKVersion(filepath.Join(s.Root, ClaudeServeDir)),
		Latest:    s.latest.get(time.Now()),
	}
}

// AskNPMForLatestSDK has SDKVersions name the newest SDK on npm. The
// daemon turns it on; a store without it (every test) never leaves the host.
func (s *Store) AskNPMForLatestSDK() { s.latest = &latestCache{fetch: fetchLatestSDK} }

// sdkLatestURL is npm's record of the newest published SDK.
const sdkLatestURL = "https://registry.npmjs.org/" + sdkPackage + "/latest"

// latestCache holds the newest SDK version npm named, asked again every
// refreshEvery; a failed ask keeps the last answer and waits as long.
type latestCache struct {
	mu       sync.Mutex
	version  string
	asked    time.Time
	inFlight bool
	fetch    func() (string, error)
}

const refreshEvery = 6 * time.Hour

// get is the cached answer; nil (not asked for) is always "".
func (c *latestCache) get(now time.Time) string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.inFlight && now.Sub(c.asked) >= refreshEvery {
		c.inFlight, c.asked = true, now
		go c.refresh()
	}
	return c.version
}

func (c *latestCache) refresh() {
	v, err := c.fetch()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight = false
	if err == nil && v != "" {
		c.version = v
	}
}

func fetchLatestSDK() (string, error) {
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(sdkLatestURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npm registry: %s", resp.Status)
	}
	var body struct {
		Version string `json:"version"`
	}
	err = json.NewDecoder(resp.Body).Decode(&body)
	return body.Version, err
}
