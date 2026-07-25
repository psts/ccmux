# Multi-host plan (federation)

Run `ccmuxd` on many machines and see every host's sessions through one lens (Mac app,
web, phone), with no notion of "which host" in the day-to-day interface. A repo on host A
and a repo on host B both live under the window `CHARTLABS`; the only place the host is
ever named is one read-only line in the workspace context menu.

The shape is **split-plane**:

- **Control plane → the hub.** One `ccmuxd` is designated the *hub*. It serves the web/PWA
  shell, aggregates every host's workspaces into one list, owns the peers directory, owns
  the dev-hostname registrar, and owns push. Every lens points at exactly one origin — the
  hub — for REST, the events firehose, peers, and push.
- **Data plane → direct to the host.** Only the terminal byte stream (`GET /v1/attach`)
  connects lens → owning host directly over the tailnet. This is the one latency-sensitive,
  identity-sensitive path, and keeping it direct preserves the zero-auth WhoIs identity
  model (each host still `tailscale whois`-es the real user) and adds no relay hop to
  keystrokes. Everything else (light metadata) is hub-fronted, so the lens keeps its single
  events socket and single peers socket.

Everything stays inside the tailnet; the tailnet remains the auth boundary. A new host is
`install ccmuxd + one TS_AUTHKEY + tag it tag:ccmux` — the hub discovers it. No fallback when
the hub is down: the control plane is simply unavailable until the hub is back (terminal
sessions themselves keep running in tmux and revive as today).

## Decisions behind this (2026-07-25)

- **Groups are global, host is invisible.** A window (`Group`) can hold workspaces from
  multiple hosts; the merged list buckets by the `Group` string that's already on the
  workspace (`model.go:52`). Host surfaces only as a context-menu line. This is *simpler*
  than per-host groups, and it forces the peers group boundary to be global — which is why
  the peers directory must live at the hub.
- **The hub is designated, not elected.** Same binary; "hub" is a role (a flag or the tag
  `tag:ccmux-hub`), not "first one started wins." First-wins races on reboot and would need
  leader-election/split-brain handling for a self-healing property we don't want (see no-
  fallback). A single-machine install runs its one daemon *as* the hub (superset role) —
  backward-compatible with today.
- **Dev hostnames are a flat global namespace.** Pick any name; the hub is the registrar and
  rejects a name already taken anywhere, with a warning. No per-host prefix. In custom-domain
  mode the wildcard DNS is singular, so the hub is the sole TLS terminator and reverse-proxies
  dev traffic to the owning host over the tailnet (two tailnet-local hops — acceptable, it's
  dev HTTP not keystrokes). ts.net fallback mode stays direct and is auto-unique via MagicDNS.
- **Settings cascade, scope travels with the value.** `effective = hostOverride ?? hubDefault`.
  Each setting carries its own scope as data, so a setting can change tier later without a
  wire-contract break.
- **No hub-down fallback** (explicitly out of scope). Terminals survive; peers, push, and
  custom-domain dev serving pause until the hub returns.
- **Version skew is a first-class handshake.** The hub gates on a `contract` integer (bumped only
  on wire-breaking changes, distinct from build version) read from each host's `/v1/health`. A
  host on a different contract is *degraded to list-only + attach*, never silently broken and never
  able to break the merged view. Built in from day one — painful to retrofit.
- **`host` is the MagicDNS label.** Human-readable, and since `Host` is runtime-only (§1) a node
  rename just re-labels on the next aggregation — nothing dangles.
- **Hosts hold no secret.** The hub is the sole peers-token authority; a host mints a pane's token
  by an authenticated call to the hub, authorized by its `tag:ccmux` tailnet identity (§ Roles).
  Consistent with "the tailnet is the auth boundary" — no secret is ever distributed.

## Roles & discovery

- **One binary, two roles.** `ccmuxd -hub` (or the node tagged `tag:ccmux-hub`) runs the hub
  services *in addition to* being a normal host. Every other node runs host-only and needs
  zero hub config — it doesn't even know the hub's address.
- **The hub discovers hosts in-process.** `serveTailnet` already holds a tsnet `LocalClient`
  (`cmd/ccmuxd/main.go:167`). The hub polls `lc.Status()`, filters peers carrying `tag:ccmux`,
  and health-probes each peer's MagicDNS name at `:443/v1/health` (the endpoint already exists,
  `server.go:96`). A healthy peer joins the registry; a failed probe greys it out. Tag a new
  box → it appears. No Tailscale API key, no host-side registration.
- **The hub is the sole peers-token authority — no secret is distributed.** The `peers-secret`
  (`main.go:75`) lives only at the hub. A host doesn't mint tokens; at pane-create it asks the hub
  to (`POST /v1/peers/pane-token {paneId}` → `{token}`), and the hub authorizes the request because
  the caller is a `tag:ccmux` tailnet peer (WhoIs). The hub still computes `TokenForPane`
  (`token.go:53`); the host just relays the result into pane env. Joining a host is therefore only
  "tag it `tag:ccmux` + it can reach the hub" — no out-of-band key. (A compromised tag:ccmux node
  could request a token for an arbitrary paneId, but such a node is already inside the trust
  boundary; this widens nothing.)

## Daemon

### 1. Host identity on the wire (runtime-only, no migration)

- `model.Workspace` and `model.Pane` gain `Host string \`json:"host,omitempty"\`` — runtime-only,
  stamped by the hub as it merges (exactly like `Workspace.Git`, `model.go:51`; never persisted,
  because a workspace's host is implicit — it came from that host's daemon). A host daemon
  leaves it empty; the hub fills it with the host's **MagicDNS label**, re-stamped from live
  `lc.Status()` each aggregation (so a node rename just re-labels, nothing dangles).
- Hosts are permanent: a live tmux session can't move between machines, so `Host` is a
  create-time, immutable attribute. Nothing in the API lets you change it.

### 2. Hub aggregation endpoints

The hub adds, alongside the existing routes (`server.go:94`):

| route | behavior |
|---|---|
| `GET /v1/hosts` | registry: `[{id, name, healthy, lastSeen}]` from tag-discovery + probes |
| `GET /v1/workspaces` | **overridden at the hub**: fan out to every healthy host's `/v1/workspaces`, merge, stamp `host` on each; group-by is unchanged (the `Group` label) |
| `GET /v1/events` | **overridden at the hub**: the hub holds one upstream events socket per host, re-emits every event with a `host` tag on one downstream firehose |
| `GET /v1/hosts/{host}/projects` | proxy `GET /v1/projects` on that host (its `-projects-root`, `main.go:47`) |
| `POST /v1/hosts/{host}/workspaces` | proxy create to the chosen host |
| everything else `/v1/workspaces/{id}/...` | the hub proxies by looking up the workspace's owning host and forwarding |

The hub is a reverse proxy for the whole `/v1` surface *except* `GET /v1/attach`, which the lens
dials on the owning host directly. Proxying uses the hub node's tailnet identity; the host
WhoIs-es the hub for these control calls (fine — they're not the terminal stream).

**Version-skew handshake.** `GET /v1/health` gains `{ok, version, contract}`:

- `version` — informational build string (e.g. git-describe), shown in UI, never gated on.
- `contract` — an integer wire-protocol version, defined once in a shared constant and bumped
  **only** on a breaking change to the `/v1` surface. Patch/feature releases that don't break the
  wire keep the same `contract`, so they never trip the handshake.

The hub compares each host's `contract` to its own on every probe and classifies the host:

| condition | `compat` | effect on the merged view |
|---|---|---|
| `host.contract == hub.contract` | `ok` | full — all endpoints proxied |
| `host.contract != hub.contract`, within a supported floor | `degraded` | **list-only + attach**: workspaces still appear and terminals still attach (both contract-stable); every mutating/feature endpoint is refused with a reason |
| below the hard floor | `unsupported` | listed as unreachable with "upgrade required"; not proxied |

`GET /v1/hosts` carries `contract`, `compat`, and a human `reason` so a lens can badge a host
("host B needs upgrade") instead of a feature silently failing. The rule is symmetric — an older
*hub* fronting a newer host degrades the same way, prompting the hub upgrade. Attach and workspace-
listing are the two shapes we commit to keeping contract-stable, since they're what a degraded host
must still serve.

### 3. Peers directory federation (the core change)

Today each daemon runs its own `peers.Service` (local `s.peers` map, local `peer_events` log,
pane thin-clients connect to the *local* daemon's `/v1/peers/ws`). Federated:

- **The hub runs the one authoritative `peers.Service`** — directory, event log, and durable
  per-peer queues (the "addressable across restarts" guarantee, `registry.go:107`, now survives
  any single host reboot because it lives at the hub).
- **Pane thin-clients connect to the hub, not their local daemon.** The host daemon injects the
  *hub's* URL + a hub-minted token into pane env instead of its own loopback. The token comes from
  the hub (§ Roles: `POST /v1/peers/pane-token`, host authorized by tailnet tag), so `ExtraPaneEnv`
  (`service.go:161`) becomes an async hub call at pane-create — rare, not a hot path; if the hub is
  unreachable at create the pane still spawns and joins peers once the hub returns (matches no-
  fallback). The env keys are unchanged (`CCMUX_PANE_TOKEN`); pane-less sessions get the URL + the
  hub's paneless token in their daemon-info file (`token.go`). Only the target and the mint source
  move.
- **Peer ids stay globally unique**: `derivedID(paneID)` over UUID pane ids (`registry.go:78`) —
  no cross-host collision. Each `Peer` gains `Host` for `list_peers` display and the context-menu
  line.
- **The group check is now global** (`messages.go:53`). The hub resolves a peer's group by joining
  `peer.PaneID → workspace → Group` against its own aggregated workspace list — so two Claudes in
  `CHARTLABS` on different hosts share a group and *can* message. Mac-local (driver) panes keep
  pushing their groups via `SetLocalPaneGroups` (`service.go:190`), now to the hub.
- **Addressing is unchanged for Claude.** It still calls `send_message` with `to_name`/`to_id`;
  the hub resolves and routes to the owning host's live connection. The existing ambiguity guard
  (`messages.go:96`) carries over — `list_peers` just shows `host` so a human picking `to_id` can
  tell same-named peers apart.
- **Standalone stays intact.** A daemon that is the hub runs the bus; a non-hub host does not.
  A lone machine is its own hub, so today's single-box behavior is unchanged.

### 4. Dev-hostname registrar + cross-host wildcard proxy

- **The hub is the registrar.** `PUT /v1/workspaces/{id}/hostnames` is proxied through the hub,
  which checks the name against the **global** map (aggregated from every host's
  `Workspace.Hostnames`, `model.go:57`) and rejects a taken name with a warning before forwarding
  the mutation to the owning host to persist. Flat namespace, no prefix.
- **Custom-domain mode**: the `*.<devDomain>` A-record self-heals to the **hub's** tailnet IP
  (the devhost DNS logic moves to the hub — `internal/devhost/server.go:36`, `ensureDNSLocked`).
  The hub holds the certmagic wildcard cert and terminates TLS, then reverse-proxies by Host header
  to the owning host over the tailnet, which runs its *existing* Host→`127.0.0.1:port` table
  (`devhost/table.go`) unchanged:
  `https://chartlabs-app.dev.sanlabs.io` → hub → `hostB.ts.net` → `127.0.0.1:3001` on host B.
- **ts.net fallback mode**: each hostname stays its own tsnet node (`server.go:91`), globally unique
  via MagicDNS, served **directly** by the owning host — no hub hop, collisions impossible.
- `cloudflare_token` becomes **hub-only** (the hub is the sole wildcard terminator); hosts no longer
  need it in custom-domain mode.

### 5. Settings cascade (self-scoped)

- The hub's `GET /v1/settings` returns a map keyed by setting name, each value
  `{value, scope, source}` where `scope ∈ global | global-default+host-override | host-local`.
  `effective = hostOverride ?? hubDefault`; `source` tells the UI "inherited from hub" vs
  "overridden on this host." Per-host overrides live at `/v1/hosts/{host}/settings`.
- Classification of today's keys (`server.go:148`):

  | key | scope |
  |---|---|
  | `dev_domain` | global, singular |
  | `cloudflare_token` | hub-only |
  | `startup_command` | global default + host override |
  | `startup_rules` | host-local (keyed by host repo paths) |
  | `tailscale_authkey` | host-local |
  | `-projects-root` (launch flag) | host-local |

  Scope travels with the value, so re-tiering a key later is a data change, not a schema break.

### 6. Push + presence at the hub

- One VAPID keypair and one `push_subscriptions` table (`model.go:117`) at the hub. The hub's
  notifier subscribes to its own aggregated events firehose (§2), dedups, and sends — replacing
  the per-daemon `EnablePush` wiring (`main.go:117`) on non-hub hosts.
- **Presence aggregates to the hub even though terminal bytes stay direct.** A host owns the direct
  attach connection, so it knows who's watching; it reports attach/detach presence up to the hub so
  the hub's notifier can apply the same per-device suppression as today (commit `4603e3c`). This is
  the one place the direct data plane still has to feed the hub.

## Lenses

Split for both lenses: **REST + events + peers + push → hub origin; `attach` → owning host.**

### 7. Mac app

- `DaemonConfig.baseURL` (`Services/DaemonConfig.swift:12`) points at the hub. Unchanged for REST,
  events, peers, push.
- `DaemonAttachClient` derives the terminal WS origin from the pane's new `host` field
  (`wss://<host>.ts.net/v1/attach`), not the single base URL — the one place the app talks to a host
  directly.
- Create flow (§2 / decision #2): `GET /v1/hosts` → host picker → `GET /v1/hosts/{host}/projects`
  → repo picker → `POST /v1/hosts/{host}/workspaces`. Extends `HostedProjectPickerView` with a host
  step.
- Context menu gains one read-only line, "Hosted on: `<host>`", from `DaemonWorkspace.host`
  (`hostedContextMenu`, `SidebarView.swift`).

### 8. Web lens

- Served by the hub. Every same-origin `fetch`/`WebSocket` (`web/app.js`) already hits the hub —
  no change for REST/events/peers/push.
- The **one** change: the terminal attach socket uses the pane's `host` instead of `location.host`
  (`app.js:512`) → `wss://<host>.ts.net/v1/attach`. This is already viable — the daemon's upgrader
  allows cross-origin (`CheckOrigin` returns true, `server.go:62`), ts.net certs are real LE certs,
  and MagicDNS resolves each host by name on any tailnet device. Host picker added to the create
  sheet; context-menu host line mirrors the app.

## Verification & rollout

1. **Two-host bring-up.** Machine 1: `ccmuxd -hub`, tagged `tag:ccmux tag:ccmux-hub`. Machine 2:
   `ccmuxd`, tagged `tag:ccmux` (no secret to distribute). Confirm machine 2 appears in
   `GET /v1/hosts` within one probe interval and its workspaces show in the merged list, host-tagged.
2. **One window, two hosts.** Put a repo on each machine under `Group:"CHARTLABS"`; confirm both
   render under one window in every lens and the context menu names the right host for each.
3. **Terminal is direct.** Attach a pane on machine 2 from the Mac app and from an iPhone; confirm
   the WS goes to `machine2.ts.net` (not the hub) and identity resolves to the real tailnet user.
4. **Cross-host peers.** Two Claudes, one per host, same window; `send_message to "<name>"` routes
   across hosts; `list_peers` shows host; verdict relay still works; queue survives restarting the
   *host* (not just the pane).
5. **Global dev hostname.** Claim `chartlabs-app` on host A; claiming it again on host B warns "taken";
   open `https://chartlabs-app.dev.sanlabs.io` and confirm it reaches host A's `:3001` through the hub.
6. **Hub down.** Kill the hub: terminals already attached keep streaming (direct); the session list,
   peers, and push pause; restarting the hub restores them. No fallback expected.
7. **Single machine unchanged.** One daemon with the hub role behaves exactly as today.
8. **Version skew.** Run a host built one `contract` behind the hub: `GET /v1/hosts` shows it
   `degraded` with a reason; its workspaces still list and its panes still attach; a mutating call
   is refused with the reason, not a 500. Bump it back to matching → `compat: ok`, full surface.

## Order of work (agent wall-clock)

1. `Host` field + hub reverse-proxy skeleton + `GET /v1/hosts` + tag discovery/probe (~60 min)
2. Aggregated `GET /v1/workspaces` + fan-in events firehose (~45 min)
3. Peers directory federation: pane env → hub URL, global group resolve, `Host` on `Peer` (~90 min)
4. Dev-hostname registrar (global uniqueness) + hub wildcard terminator/proxy; move DNS self-heal
   to hub (~75 min, + ~2 min cert issuance)
5. Settings cascade endpoint + scope tags (~45 min)
6. Push + presence relocation to hub; host→hub presence feed (~45 min)
7. Mac app: hub base + direct-attach-by-host + host picker + context-menu line (~60 min)
8. Web: attach-by-host + create host picker + context-menu line (~30 min)
9. Two-host E2E + `multihost-runbook.md` (tag setup, secret distribution, hub role) (~45 min)

≈ 8 h of execution. External waits are wildcard-cert issuance and the one-time per-host tag +
secret distribution (user steps). No launchd changes on existing single-host installs; adding a
host is a new node with the `tag:ccmux` tag and the shared `peers-secret`.

## Resolved (previously open)

- **Secret distribution** → none. Hosts hold no secret; the hub is the sole token authority and
  mints per pane, authorized by tailnet tag (§ Roles, § 3). Joining a host is just tagging it.
- **Version skew** → the `contract` handshake in § 2, built in from day one.
- **Host id** → the MagicDNS label (§ 1); safe because `Host` is runtime-only and re-stamped.

## Still open (decide during build, not blocking)

- **Supported-floor width.** How many `contract` versions back the hub tolerates as `degraded`
  before `unsupported`. Start at "exactly one behind"; widen only if upgrades prove staggered.
- **Paneless token freshness.** The hub's paneless token is shared; mint it per-session vs. let the
  host cache it in the daemon-info file. Cache is simpler; revisit if it needs rotation.
