package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"

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

// lensURLFile is where the link a browser on this machine opens is kept.
func lensURLFile() string { return filepath.Join(privateDir(), "lens-url") }

// mintAPIToken makes this start's API token and writes it to its file,
// with the link that carries it beside it. base is the daemon's own
// loopback address ("http://127.0.0.1:7900").
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
	for file, text := range map[string]string{apiTokenFile(): token, lensURLFile(): lensURL(base, token)} {
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
// with the token in it.
func lensURL(base, token string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	u.Host = api.LensHost + ":" + u.Port()
	u.Path, u.RawQuery = "/", "key="+token
	return u.String()
}

// cmdAPIToken prints the running daemon's API token: what a script on this
// machine sends as a bearer token.
func cmdAPIToken([]string) error { return printPrivate(apiTokenFile()) }

// cmdLensURL prints the link a browser on this machine opens the lens
// with.
func cmdLensURL([]string) error { return printPrivate(lensURLFile()) }

func printPrivate(file string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("%w (is the daemon running, and are you outside the sandbox? A walled session cannot read it)", err)
	}
	fmt.Print(string(b))
	return nil
}
