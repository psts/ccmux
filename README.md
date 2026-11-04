# ccmux

One view of every Claude Code session you run, from any browser or your phone.

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
- **The lens** is the web app (a PWA) the daemon serves. Open it in a browser, or install
  it as an app on your computer or phone. It renders your sessions and streams the
  terminal; the work itself runs in the daemon's tmux, not in the lens.

Multiple hosts federate through a designated **hub**: one `ccmuxd` aggregates every host's
sessions into one list, owns the peer-messaging bus, and fronts push — while the terminal
byte stream still connects straight to the owning host. See
[`daemon/docs/multihost-plan.md`](daemon/docs/multihost-plan.md).

## Install a host

**Releases stopped at v0.1.88 (2026-11-02).** The installer below still works but
installs that version. For anything newer, build from a checkout: `./build-daemon.sh`
builds both binaries into `~/.local/bin` and restarts an existing install's service;
on a new host, run `~/.local/bin/ccmuxd install` once after it.

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

## Update a host

To update from source, pull and run `./build-daemon.sh`. Do not run `ccmuxd upgrade`
on a source build newer than v0.1.88: it installs the latest published release, which
is older. For hosts that stay on releases, on any host running v0.1.3 or later:

```sh
ccmuxd upgrade            # to the latest release; `ccmuxd upgrade v0.1.4` pins one
```

It resolves the release, verifies the checksum, swaps both binaries in place, and
hands off to the new version's installer in update mode: your previous answers
(node name, hub role, projects root) are reused, nothing is asked, and the daemon
is restarted on both platforms. Pass install flags by re-running the curl installer
instead when you want to *change* a setting (`--hostname`, `--hub`, …).

Re-running the curl installer from the Install section is the equivalent update
path for hosts on older versions (or when `ccmuxd` isn't on PATH) — same update
mode, same kept answers; pre-v0.1.2 installs recover their answers from the
service file. It also honors `CCMUX_VERSION=vX.Y.Z` to pin a release.

One thing remains yours either way: restart your Claude Code sessions afterwards.
`ccmux-peers` runs per session, not under the service, so every session keeps its
old shim until it restarts.

Verify: `ccmuxd version`, or `curl -s http://127.0.0.1:7900/v1/health` for the
version of the *running* daemon.

## Open the lens

The daemon serves the lens itself. Open the daemon's address in a browser: on a tailnet
host that is `https://<host-label>.<tailnet>.ts.net` (the hub shows every host), locally
the installed service's `http://127.0.0.1:7900`.
To use it like an app, install it from the browser (Chrome: Install app; Safari: Add to
Dock; phone: Add to Home Screen).

The native macOS app was retired on 2026-11-04. Its code is kept on the
[`mac-app`](https://github.com/psts/ccmux/tree/mac-app) branch, frozen as it was then.

## Repository layout

```
daemon/             the ccmuxd daemon (Go)
  cmd/ccmuxd/         daemon entrypoint + `install`/`uninstall` subcommands
  cmd/ccmux-peers/    per-session peers MCP thin-client
  internal/           tmux control, REST/WS API, hub federation, peers, devhost, push
  web/                the web/PWA lens the daemon serves (no build step)
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

Run a daemon in the foreground, on a host with no installed one (it serves the web lens
on the same address):

```sh
go run ./cmd/ccmuxd -addr 127.0.0.1:7900
```

The web lens is plain JS in `daemon/web/`, embedded into `ccmuxd` at build time, so
rebuild the daemon to see a change.

Cutting a GitHub release is by hand only since 2026-11-02 (`gh workflow run release
--ref vX.Y.Z`; a pushed tag builds nothing). The workflow cross-compiles `ccmuxd` + `ccmux-peers` for darwin-arm64,
linux-amd64, and linux-arm64 and publishes the tarballs `install.sh` downloads. Validate
the release config locally first with `goreleaser check` (and dry-run with
`goreleaser release --snapshot --clean`).
