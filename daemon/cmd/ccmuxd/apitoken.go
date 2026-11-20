package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ccmux.dev/ccmuxd/internal/api"
)

// The token the daemon's API wants from a caller on this machine
// (api/localgate.go). It is made new at every start of the daemon and kept
// in a file only this user reads, in a folder the sandbox hides from every
// walled unit (sandbox.go, secrets). So a session behind walls can reach
// the daemon's port and still cannot switch the sandbox off, start a
// session outside the walls or type into one.
//
// The folder is hidden, not the file: a file that is written again may be
// a new file, which a unit that was already running then sees.

// privateDir is the folder of the daemon's runtime files that no walled
// unit may read.
func privateDir() string { return filepath.Join(runtimeDir(), "private") }

// apiTokenFile is where the API token is kept.
func apiTokenFile() string { return filepath.Join(privateDir(), "api-token") }

// apiURLFile is where the daemon's own loopback address is kept, for the
// two commands below: they run with no flags of the daemon's.
func apiURLFile() string { return filepath.Join(privateDir(), "api-url") }

// mintAPIToken makes this start's API token and writes it to its file,
// with the daemon's loopback address ("http://127.0.0.1:7900") beside it.
func mintAPIToken(base string) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("the API token could not be made: %w", err)
	}
	token := hex.EncodeToString(raw)
	if err := os.MkdirAll(privateDir(), 0o700); err != nil {
		return "", fmt.Errorf("the API token's folder: %w", err)
	}
	// The folder's mode again: MkdirAll keeps the mode of one that was there.
	if err := os.Chmod(privateDir(), 0o700); err != nil {
		return "", fmt.Errorf("the API token's folder: %w", err)
	}
	for file, text := range map[string]string{apiTokenFile(): token, apiURLFile(): base} {
		if err := writePrivate(file, text+"\n"); err != nil {
			return "", err
		}
	}
	return token, nil
}

// armAPIToken makes this start's token and switches the API's gate on with
// it. A token that cannot be made ends the daemon: the caller must not go
// on to serve.
func armAPIToken(srv *api.Server, base string) {
	token, err := mintAPIToken(base)
	if err != nil {
		log.Fatalf("api token: %v", err)
	}
	srv.SetAPIToken(token)
	log.Printf("api: callers on this machine need the token in %s (`ccmuxd lens-url` prints the link a browser here opens)", apiTokenFile())
}

// writePrivate writes a file only this user reads.
func writePrivate(file, text string) error {
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		return fmt.Errorf("%s was not written: %w", file, err)
	}
	// The mode again: WriteFile keeps the mode of a file that was there.
	if err := os.Chmod(file, 0o600); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

// lensURL is the link a browser on this machine opens the lens with: the
// daemon's loopback address under the lens's own host name (api.LensHost),
// with a one-use key in it.
func lensURL(base, key string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	u.Host = api.LensHost + ":" + u.Port()
	u.Path, u.RawQuery = "/", "key="+key
	return u.String()
}

// cmdAPIToken prints the running daemon's API token, for a script on this
// machine to send as a bearer token. It is meant to be read where it is
// used, curl -H "Authorization: Bearer $(ccmuxd api-token)", and never
// printed: what a session prints is saved in its transcript, and a walled
// session reads every transcript under home.
func cmdAPIToken([]string) error {
	token, err := readPrivate(apiTokenFile())
	if err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}

// cmdLensURL prints the link a browser on this machine opens the lens
// with. The link may be printed anywhere: its key is not the token, works
// once, and only for two minutes.
func cmdLensURL([]string) error {
	link, err := lensLink(&http.Client{Timeout: 5 * time.Second})
	if err != nil {
		return err
	}
	fmt.Println(link)
	return nil
}

// lensLink asks the running daemon for a one-use key (POST /v1/lens-key,
// with the API token) and makes the link that carries it.
func lensLink(client *http.Client) (string, error) {
	token, err := readPrivate(apiTokenFile())
	if err != nil {
		return "", err
	}
	base, err := readPrivate(apiURLFile())
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, base+"/v1/lens-key", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the daemon at %s did not answer: %w", base, err)
	}
	defer resp.Body.Close()
	var out struct{ Key, Error string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || out.Key == "" {
		return "", fmt.Errorf("the daemon at %s gave no key for a link (HTTP %d %s)", base, resp.StatusCode, out.Error)
	}
	link := lensURL(base, out.Key)
	if link == "" {
		return "", fmt.Errorf("the daemon's address %q is not one a link can be made from", base)
	}
	return link, nil
}

// readPrivate reads one of the daemon's private files, trimmed.
func readPrivate(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%w (is the daemon running, and are you outside the sandbox? A walled session cannot read it)", err)
	}
	return strings.TrimSpace(string(b)), nil
}
