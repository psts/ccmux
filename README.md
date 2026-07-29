# ccmux

One view of every Claude Code session you run, from your Mac, the web, or your phone.

ccmux keeps your Claude Code sessions alive in `tmux` on whatever machine they run on,
and gives you a single interface to attach to them from anywhere on your tailnet. Sessions
survive app restarts, network drops, and daemon bounces. Put a repo on your laptop and a
repo on a server, and they show up side by side under one window, with no notion of "which
host" in the day-to-day interface.

## How it works

ccmux is split into a **daemon** and one or more **lenses**.

- **`ccmuxd`** (the daemon, Go) owns a dedicated `tmux` server that holds your persistent
  Claude Code sessions, and serves a REST + WebSocket API. With `-tsnet` it comes up as its
  own [Tailscale](https://tailscale.com) node, so it identifies every caller by their real
  tailnet identity (`tailscale whois`) with no passwords to manage. The tailnet is the auth
  boundary.
- **A lens** is any client that attaches to the daemon: the native macOS app, or the
  web/PWA the daemon serves. A lens renders your sessions and streams the terminal; the work
  itself runs in the daemon's tmux, not in the lens.

Multiple hosts federate through a designated **hub**: one `ccmuxd` aggregates every host's
sessions into one list, owns the peer-messaging bus, and fronts push — while the terminal
byte stream still connects straight to the owning host. See
[`daemon/docs/multihost-plan.md`](daemon/docs/multihost-plan.md).

## Install a host

Each host runs `ccmuxd` as a user service. The installer downloads the binaries, writes the
service (launchd on macOS, systemd `--user` on Linux), joins the tailnet, and registers the
per-host peers MCP:

```sh
curl -fsSL https://raw.githubusercontent.com/psts/ccmux/main/install.sh | sh -s -- \
  --hostname <host-label> --authkey "$TS_AUTHKEY"
```

Run it with no `--` args to be prompted instead. To make one node the hub, add `--hub`.
Then tag the node `tag:ccmux` (and `tag:ccmux-hub` for the hub) in the Tailscale admin
console — that's the one step that can't be scripted. Full deployment steps, including the
lens configuration and verification, are in
[`daemon/docs/multihost-runbook.md`](daemon/docs/multihost-runbook.md).

Requirements on a host: `tmux` ≥ 3.3 and `git` on `PATH`, and HTTPS certificates enabled
for your tailnet (Admin → DNS → HTTPS Certificates).

## Build the Mac app

The lens is a native SwiftUI app (macOS 14+):

```sh
./build-app.sh          # → .build/release/ccmux.app
```

By default it talks to a local daemon at `http://127.0.0.1:7900`. Point it at a remote
host (usually the hub) by setting `CCMUXD_URL`:

```sh
CCMUXD_URL=https://hub.<tailnet>.ts.net open .build/release/ccmux.app
```

## Repository layout

```
Sources/ccmux/      native macOS lens (SwiftUI)
daemon/             the ccmuxd daemon (Go)
  cmd/ccmuxd/         daemon entrypoint + `install`/`uninstall` subcommands
  cmd/ccmux-peers/    per-session peers MCP thin-client
  internal/           tmux control, REST/WS API, hub federation, peers, devhost, push
  web/                the web/PWA lens the daemon serves
  docs/               design + runbooks (multi-host, dev hostnames, peers)
hooks/              Claude Code hook scripts (attention fan-out, co-author)
install.sh          the curl|sh installer (downloads a release, runs `ccmuxd install`)
.goreleaser.yaml    release build config (ccmuxd + ccmux-peers, 3 targets)
```

## Development

The daemon is pure Go (no cgo), so it cross-compiles cleanly to any target:

```sh
cd daemon
go build ./cmd/ccmuxd            # build the daemon
go build ./cmd/ccmux-peers       # build the peers MCP client
go test ./...                    # run the daemon test suite
go vet ./...
```

Run a daemon locally on the port the app expects:

```sh
./cmd/ccmuxd -addr 127.0.0.1:7900
```

Cutting a release: push a tag (`git tag v0.1.0 && git push origin v0.1.0`) and the
GitHub Actions workflow cross-compiles `ccmuxd` + `ccmux-peers` for darwin-arm64,
linux-amd64, and linux-arm64 and publishes the tarballs `install.sh` downloads. Validate
the release config locally first with `goreleaser check` (and dry-run with
`goreleaser release --snapshot --clean`).
