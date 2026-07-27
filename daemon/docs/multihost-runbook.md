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
- **Cross-host peers bus** (built; **needs live two-host validation**): a non-hub host
  discovers the hub (`tag:ccmux-hub`) and points its Claude panes' bus at it, so a peer
  on host B can `send_message` to a peer on host A that shares a window group. The hub
  owns the directory + global group resolution; it mints each pane's token (no secret
  distributed) and accepts bus connections only from loopback or registered member-host
  IPs. When no hub is found (or it's unreachable at spawn), a host falls back to its
  local bus — so single-host is unchanged. Requires the hub tagged `tag:ccmux-hub`.

- **Per-host settings** + **global dev-hostname uniqueness** (built): the lens
  configures any host via `GET/PUT /v1/hosts/{host}/settings`, and the hub rejects a
  dev-hostname label already claimed by another workspace on any host ("taken on host X").

## Not yet federated

- **Dev-hostname reachability across hosts** — uniqueness is enforced, but the hub
  wildcard-cert terminator that reverse-proxies `*.<devDomain>` to the owning host
  (`multihost-plan.md` §4) is pending (needs live certs/DNS). Today a dev hostname is
  only reachable on the host that serves it.
- **Settings cascade** — per-host access works; hub-default → host-override inheritance
  with scope tags (§5) is pending.
- **Push** — still per-daemon VAPID/notifier; the hub relocation (§6) is pending and
  blocked on aggregating focus/presence to the hub (so remote-session suppression stays
  correct — otherwise unified push would over-notify).

Because every unshipped piece is additive and gated behind hub mode, a single-host
install behaves exactly as before.

## Deploy

### 1. Every host (including the future hub)

Install `ccmuxd`, run it as its own tailnet node, and tag it:

```
ccmuxd -tsnet -tsnet-hostname <host-label> [-projects-root /srv/projects]
```

In the Tailscale admin console (or ACL policy), give each node the tag **`tag:ccmux`**.
First run needs `TS_AUTHKEY` (a reusable, pre-approved key, ideally auto-tagged
`tag:ccmux`). HTTPS certs must be enabled for the tailnet (Admin → DNS → HTTPS
Certificates) — the same requirement single-host `-tsnet` already carries.

No secret is distributed to hosts: joining a host is *tag it + let it reach the hub*.

### 2. The hub (one node — a normal host plus the hub role)

Pick one node, add `-hub`, and tag it `tag:ccmux-hub` (the tag is what member hosts
discover to federate their peers bus — not optional if you want cross-host peers):

```
ccmuxd -tsnet -tsnet-hostname hub -hub [-projects-root /srv/projects]
```

`-hub` requires `-tsnet`. The hub discovers members from the tailnet on a 5s probe;
tag a new box `tag:ccmux` and it appears within one cycle. A single-machine install
runs its one daemon as the hub (superset role) — nothing else to do. Member hosts
need no hub config: they find `tag:ccmux-hub` themselves and mint pane tokens from it.

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
   host B's logs for `peers: federating to hub …` and that its panes carry
   `CCMUX_PEERS_URL` (hub) while `CCMUX_DAEMON_URL` stays local.

## Notes

- **Hub down**: attached terminals keep streaming (direct); the merged list, events,
  and (once federated) peers/push pause until the hub returns. No fallback by design.
- **Host rename**: `host` is the MagicDNS label, re-stamped each aggregation from live
  tailnet status, so a rename just re-labels — nothing dangles.
- **Build version**: set `-ldflags "-X ccmux.dev/ccmuxd/internal/version.Build=$(git describe)"`
  so `/v1/health` and the UI report a real build id (defaults to `dev`).
