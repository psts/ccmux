# Dev hostnames plan (devhosts)

Serve hosted workspaces' dev servers at stable HTTPS names over the tailnet, configured
per-workspace from the app (right-click → Hostnames…). Two serving modes, selected by one
daemon setting:

- **Custom-domain mode** (when `devDomain` is set, e.g. `dev.sanlabs.io`):
  `https://chartlabs-app.dev.sanlabs.io` → `127.0.0.1:3001` on the daemon host.
  One wildcard cert (`*.devDomain`) via Let's Encrypt DNS-01 with a Cloudflare API token;
  served from the existing ccmuxd tsnet node's :443, dispatched by SNI/Host next to the API.
  The daemon self-heals the `*.devDomain` A record → its own tailnet IP through the same token.
- **ts.net fallback mode** (no `devDomain`): each hostname becomes its own tsnet node —
  `https://chartlabs-app.tailb9053d.ts.net` — with Tailscale-issued certs, zero DNS anywhere.
  Registration is silent when a `tailscaleAuthKey` setting is present (else the auth URL is logged).

Decisions behind this (investigated 2026-07-16): CNAME → ts.net is impossible — non-Funnel
ts.net names are NXDOMAIN in public DNS (verified against live nodes); Funnel would both
publish the service and hit the SNI-routing failure of tailscale/tailscale#13029; custom
domains on Funnel are an unimplemented FR (#11563). Tailnet IPs are stable, so a wildcard
A record + self-heal fully replaces the CNAME idea. Everything terminates TLS in ccmuxd;
Tailscale is only the network.

## Daemon

### 1. Settings (existing KV `settings` table + `/v1/settings`)

New keys, following the `startup_rules` pattern (`manager.go:142`):

| key | meaning |
|---|---|
| `dev_domain` | e.g. `dev.sanlabs.io`; empty = ts.net mode |
| `cloudflare_token` | zone-scoped DNS-edit token; required when `dev_domain` set |
| `tailscale_authkey` | optional; reusable pre-approved key for fallback-mode node registration |

- PUT validates: domain without token → 400. Secrets are write-only: GET returns
  `cloudflareTokenSet` / `tailscaleAuthKeySet` booleans plus `devDomain` and a
  `devCertStatus` string (`unset|pending|ready|error: …`), never the values.
  Explicit `""` on PUT clears a secret.
- Registry file already lives under `~/Library/Application Support/ccmuxd/`; ensure 0600 on the DB.

### 2. Model + store

- `model.Hostname{Name string, Port int}`; `Workspace.Hostnames []Hostname` (persisted) plus
  runtime-only serialized fields per entry: `url` (computed from mode) and `listening` (probe).
- Store: `hostnames_json TEXT DEFAULT ''` column via the idempotent `ALTER TABLE` migration
  pattern (`store.go:96`), included in the upsert/scan; `SetWorkspaceHostnames(id, json)`
  mirroring `SetWorkspaceGroup` (`store.go:190`).

### 3. API

- `PUT /v1/workspaces/{id}/hostnames` (mimics `putGroup`, `server.go:100`), body `[{name, port}]`.
  Validation: label regex `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`, port 1–65535, name unique
  across **all** workspaces. Returns the updated workspace.
- Reachable tailnet-wide like other workspace mutations — consistent trust model (WhoIs identity).

### 4. New package `daemon/internal/devhost/`

- `table.go` — immutable `Table` rebuilt from manager state; `Route(host) (port, ok)` strips
  the port, folds case, keys on full FQDN. Pure dispatch — **test-first**.
- `proxy.go` — `httputil.ReverseProxy` → `127.0.0.1:<port>`; friendly 404 for unknown hosts
  (lists known hostnames), 503 while the cert/backend isn't ready. WS upgrades pass through.
- `domain.go` — certmagic config: `DNS01Solver` + libdns/cloudflare, storage
  `configDir()/certmagic`, resolvers pinned to public DNS (1.1.1.1) so the host's MagicDNS
  can't confuse propagation checks (garrido's gotcha), manages `*.<devDomain>` async with
  status surfaced as `devCertStatus`.
- `dns.go` — A-record self-heal: on boot + settings change, upsert `*.<devDomain>` A →
  ccmuxd node's tailnet IPv4 (from `ts.TailscaleIPs()`), DNS-only (never proxied). Zone =
  registrable suffix of `devDomain` by default (walk suffixes on API error).
- `tsnodes.go` — fallback mode: reconcile loop diffing desired hostnames vs running
  `tsnet.Server`s (`Hostname: <name>`, `Dir: configDir()/tsnet-hosts/<name>`,
  `AuthKey` from settings, `ListenTLS(":443")` → shared proxy handler). Spin up on add/boot,
  `Close()` + state-dir removal on delete.
- `devhost.Server.Apply(settings, table)` — idempotent mode reconcile, called at boot and on
  any settings/hostnames mutation (direct call from manager; no event bus needed).

Deps to add: `github.com/caddyserver/certmagic`, `github.com/libdns/cloudflare`.

### 5. Listener integration (custom-domain mode)

Replace `ts.ListenTLS(":443")` in `serveTailnet` (`cmd/ccmuxd/main.go:153`) with
`ts.Listen("tcp", ":443")` wrapped in `tls.NewListener` whose `GetCertificate` dispatches by
SNI: `*.<devDomain>` → certmagic, else → tsnet `local.GetCertificate` (ts.net cert, verified
available in tailscale.com v1.100.0). The HTTP handler dispatches by Host: table hit → proxy,
else → existing API handler. No new node, no host-port claims; the A record points at the
ccmuxd node (today 100.80.17.39).

### 6. Status probe

`listening` per mapping: TCP dial `127.0.0.1:<port>` with ~200 ms timeout, cached ~5 s,
computed when serializing workspaces (same runtime-only spirit as `Workspace.Git`).

### 7. Tests

- `table_test.go`: dispatch table (host:port stripping, case, unknown host, fqdn keying) — test-first.
- API: hostnames PUT validation + cross-workspace uniqueness (pattern: `settings_test.go`).
- Store: column round-trip (pattern: `group_test.go`).
- Proxy: `httptest` backend behind the table, incl. an upgrade request.
- `domain.go`/`dns.go`: mock the libdns provider interface; never hit Cloudflare in tests.

## Mac app

### 8. Settings window

`DaemonSettingsView.swift` gains a "Dev hostnames" section (flat `VStack` + headline, like the
startup sections): domain TextField, Cloudflare token SecureField, Tailscale auth key
SecureField, mode caption ("domain set → …, empty → ts.net"), and the `devCertStatus` line.
Extend `DaemonSettings` (`DaemonModels.swift:50`) + `updateSettings` body keys
(`RemoteSessionService.swift:221`); secrets sent only when the field was edited.

### 9. Hostnames sheet

- Menu item "Hostnames…" in `workspaceContextMenu` / `hostedContextMenu`
  (`SidebarView.swift:333-371`) via a new `onWorkspaceHostnames: (UUID) -> Void` closure
  threaded from `WorkspaceWindowController` (same style as `onAddWorkspace`).
- Sheet presented AppKit-style: `NSHostingController` + `window.beginSheet`
  (mimic `showAddHostedWorkspacePanel`, `WorkspaceWindowController.swift:63-84`; visual
  template `HostedProjectPickerView`). Rows: name field (prefilled from workspace slug),
  port field, live URL preview, delete; Cancel/Save → `PUT /v1/workspaces/{id}/hostnames`.

### 10. URL surfacing

`DaemonWorkspace` gains `hostnames: [{name, port, url, listening}]`. Show each URL in the
workspace's expanded dashboard body with a reused `ConnectionDot` (`SidebarView.swift:437`)
for `listening`; click opens the browser, ⌥-click copies.

## Verification & rollout

1. Daemon unit + race suites; app builds.
2. E2E (custom mode): set domain + token in Settings → `python3 -m http.server 3001` in a
   hosted workspace → add hostname → open from MBP **and** iPhone over the tailnet; confirm
   valid LE cert; confirm A record appeared in Cloudflare (grey cloud).
3. E2E (fallback): clear domain → same mapping appears as `<name>.tailb9053d.ts.net`; node
   visible in the admin console; cert issued.
4. No launchd changes (secrets travel via settings, not env). Restart:
   `launchctl kickstart -k gui/501/com.ccmux.ccmuxd`.
5. Write `daemon/docs/devhosts-runbook.md`: Cloudflare token creation (zone-scoped DNS edit),
   auth-key creation (reusable + pre-approved, tagged), first-cert wait (~1–2 min), device-approval
   caveat for fallback nodes.

## Order of work (agent wall-clock)

1. `devhost.Table` + tests (~15 min)
2. model/store/manager/API + tests (~30 min)
3. certmagic + SNI listener swap (~45 min, + ~2 min first issuance wait)
4. ts.net fallback reconciler (~30 min)
5. DNS self-heal (~20 min)
6. Settings UI (~20 min)
7. Hostnames sheet + menu + URL display (~45 min)
8. E2E + runbook (~30 min)

≈ 4 h of execution; external waits are cert issuance and the one-time Cloudflare token /
auth key creation (user step).
