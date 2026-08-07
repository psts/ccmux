# Multi-host runbook (federation)

How to deploy the federation and what's live today. Design: `multihost-plan.md`.

## What's implemented (this is deployable now)

- **Hub role** (`ccmuxd -hub`, requires `-tsnet`): discovers `tag:ccmux` tailnet
  peers, aggregates every host's workspaces into one host-stamped `GET /v1/workspaces`,
  exposes the registry at `GET /v1/hosts`, merges every host's event firehose, and
  reverse-proxies workspace/pane and `/v1/hosts/{host}/*` routes to the owning host.
- **Version-skew handshake**: `GET /v1/health` carries `{ok, version, contract}`;
  mismatched hosts degrade to list-only + attach (mutations refused), never break
  the merged view.
- **Direct-attach lenses**: web and Mac app attach the terminal stream straight to
  the owning host (`wss://<host>.<tailnet>.ts.net/v1/attach`), never through the hub.
  Web has the full host-targeted create picker + host context line; Mac has the host
  context line and direct-attach (host-targeted create is a follow-up — it creates on
  the hub by default).
- **Cross-host peers bus** (built; validated live on a two-host fleet 2026-08-07): a
  non-hub host discovers the hub (`tag:ccmux-hub`) and points its Claude panes' bus at
  it, so a peer on host B can `send_message` to a peer on host A that shares a window
  group. The hub owns the directory + global group resolution; it mints each pane's
  token (no secret distributed) and accepts bus connections only from loopback or
  registered member-host IPs. When no hub is found (or it's unreachable at spawn), a
  host falls back to its local bus — so single-host is unchanged. Requires the hub
  tagged `tag:ccmux-hub`.

  A member's panes reach that bus through `POST/GET /v1/hubbus/*` on their OWN daemon,
  which relays to the hub over the daemon's tailnet connection — panes never dial the
  hub themselves. They cannot: with `-tsnet` the daemon's tailnet node and the machine's
  own tailscaled are DIFFERENT nodes with different IPs, so a pane's registration
  arrived from an address the hub had never discovered and was refused with "peers
  connection must be loopback or a member host", silently, on every attempt. The relay
  is loopback-only and forwards only the bus paths a thin client uses (`internal/api/hubbus.go`).

- **Per-host settings** + **global dev-hostname uniqueness/routing** (built): the lens
  configures any host via `GET/PUT /v1/hosts/{host}/settings`; the hub rejects a
  dev-hostname label already claimed by another workspace on any host ("taken on host
  X") and reverse-proxies a hostname owned by another host to it.
- **Hub-owned push** (built): the hub notifies on attention across every host (merged
  events) and suppresses via merged presence (it polls each member's `GET /v1/presence`),
  so a user watching a remote session directly still quiets their phone. Point the lens's
  push subscription at the hub.

## Remaining — live config / a device only

- **Dev-hostname reachability across hosts** — routing is built, but making a routed URL
  actually resolve needs the wildcard `*.<devDomain>` A-record pointed at the hub, the
  wildcard cert issued on the hub, and the SAME `devDomain` configured on every member
  (so the owning host's devhost table matches the proxied Host). Live certs/DNS.
- **Settings cascade inheritance** — per-host access works; hub-default → host-override
  with scope tags (§5) is an optional refinement.
- **End-to-end validation** — peers cross-host messaging and push delivery need two live
  hosts + a phone (see Verify §5 and the push note).

Because every unshipped piece is additive and gated behind hub mode, a single-host
install behaves exactly as before.

## Deploy

Every host runs the same binary; "hub" is one extra flag on one node. The installer
downloads the binaries, writes a user service (launchd on macOS, systemd `--user` on
Linux), joins the tailnet once, and registers the per-host peers MCP. It re-runs
safely.

**Prerequisite (all hosts):** HTTPS certs enabled for the tailnet (Admin → DNS →
HTTPS Certificates) — the same requirement single-host `-tsnet` already carries. Have
a reusable `TS_AUTHKEY` ready, ideally pre-approved and auto-tagged `tag:ccmux`.

### 1. Every host (including the future hub)

```
curl -fsSL https://raw.githubusercontent.com/psts/ccmux/main/install.sh | sh -s -- \
  --hostname <host-label> --authkey "$TS_AUTHKEY" [--projects-root /srv/projects]
```

Run with no `--` args to be prompted instead (override the source with `CCMUX_REPO`).
The installer joins the tailnet once — the
key is consumed into node state, never written into the service file — then serves
`127.0.0.1:7900` plus the tailnet node's `:443`.

Then, in the Tailscale admin console (or ACL policy), tag the node **`tag:ccmux`**.
No secret is distributed to hosts: joining a host is *tag it + let it reach the hub*.

### 2. The hub (one node — a normal host plus the hub role)

Pick one node and add `--hub`, then tag it `tag:ccmux-hub` (that tag is what member
hosts discover to federate their peers bus — required for cross-host peers):

```
curl -fsSL https://raw.githubusercontent.com/psts/ccmux/main/install.sh | sh -s -- \
  --hostname hub --hub --authkey "$TS_AUTHKEY" [--projects-root /srv/projects]
```

The hub discovers members from the tailnet on a 5s probe; tag a new box `tag:ccmux`
and it appears within one cycle. A single-machine install runs its one daemon as the
hub (superset role) — nothing else to do. Member hosts need no hub config: they find
`tag:ccmux-hub` themselves and mint pane tokens from it.

> **From source instead of a release:** in `daemon/`, `go build -o ~/.local/bin/ccmuxd
> ./cmd/ccmuxd && go build -o ~/.local/bin/ccmux-peers ./cmd/ccmux-peers`, then
> `ccmuxd install --hostname <label> [--hub] --authkey "$TS_AUTHKEY"`. The subcommand
> does the same service + tailnet + MCP setup on a locally-built binary.

### 3. Point lenses at the hub

- **Web / phone**: browse to `https://hub.<tailnet>.ts.net` (the hub serves the PWA).
- **Mac app**: set `CCMUXD_URL=https://hub.<tailnet>.ts.net`.

REST, events, peers, and push go to the hub; terminal attach goes direct to each
owning host (the lens resolves `host` → address from `GET /v1/hosts`).

## Verify

1. `curl -s https://hub.<tailnet>.ts.net/v1/hosts | jq` — every tagged node listed,
   `compat: "ok"`, `healthy: true`, self first.
2. `curl -s https://hub.<tailnet>.ts.net/v1/workspaces | jq '.[].host'` — workspaces
   carry the owning host's label.
3. In a lens: sessions from every host appear under one window when they share a
   `group`; the context menu shows `⬡ <host>`; attaching a remote-host session opens
   a WebSocket to that host directly (check the host's logs, not the hub's).
4. Version skew: run one host built a `contract` behind the hub → it shows `degraded`
   in `/v1/hosts`, still lists + attaches, and a mutation returns 409 with the reason.
5. Cross-host peers (the live-validation pass): a Claude on host B and one on host A in
   the same window group — `list_peers` shows both with their `host`; `send_message` to
   the other's name is delivered; a non-member tailnet node is refused at the bus. Check
   host B's logs for `federating to hub …` (no `peers:` prefix on that line) and for
   `peers: host federation armed …`. Panes carry no hub variable at all — the shim asks
   host B's daemon (`POST /v1/peers/bus`) and gets the live `tag:ccmux-hub` answer, so
   membership follows the tag rather than whatever was true when the session was
   created. `CCMUX_DAEMON_URL` stays local.

   Resolution happens on each busLoop iteration, on the keepalive tick, and every 2
   minutes from the watchdog — **not** on the reconnects inside `runPushLoop`. So a hub
   that appears while a pane holds a healthy push connection is picked up within about
   two minutes, not instantly. To confirm a move, look for `bus moved to …` in the
   pane's shim log.

## Notes

- **Hub down**: attached terminals keep streaming (direct); the merged list, events,
  and (once federated) peers/push pause until the hub returns. No fallback by design.
- **Host rename**: `host` is the MagicDNS label, re-stamped each aggregation from live
  tailnet status, so a rename just re-labels — nothing dangles.
- **Build version**: set `-ldflags "-X ccmux.dev/ccmuxd/internal/version.Build=$(git describe)"`
  so `/v1/health` and the UI report a real build id (defaults to `dev`).
