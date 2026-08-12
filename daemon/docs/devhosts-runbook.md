# Dev hostnames runbook

Serve hosted workspaces' dev servers at stable HTTPS names over the tailnet.
Feature plan: `devhosts-plan.md`. Everything is driven from the app: ccmux →
Settings… (⌘,) for the daemon-wide config, right-click a hosted workspace →
**Hostnames…** for per-workspace mappings. URLs appear in the workspace's
expanded sidebar row (dot = dev server answering) and its context menu.

## Mode 1 — custom domain (`chartlabs-app.dev.sanlabs.io`)

One-time setup:

1. **Cloudflare API token** (dash.cloudflare.com → My Profile → API Tokens →
   Create Token → "Edit zone DNS" template): permission `Zone / DNS / Edit`,
   scoped to the `sanlabs.io` zone. Copy the token.
2. In ccmux Settings: set **Custom domain** to `dev.sanlabs.io` and paste the
   token. Save.

The daemon then does everything else, and re-asserts it on every boot/change:

- Creates/repairs the DNS record `*.dev.sanlabs.io A <ccmuxd node tailnet IP>`
  (DNS-only, never Cloudflare-proxied — a 100.x address is only reachable from
  the tailnet, that's the point).
- Issues a Let's Encrypt wildcard `*.dev.sanlabs.io` via DNS-01 (~1–2 min the
  first time; Settings shows `pending → ready`). Renewals are automatic; cert
  state lives in `~/Library/Application Support/ccmuxd/devhost/certmagic`.
- Serves the names by SNI/Host dispatch on the existing ccmuxd tsnet node's
  :443, reverse-proxying to `127.0.0.1:<port>` on the daemon host.

Notes:
- The tailnet IP is stable for the node's lifetime; if the node is ever
  re-registered the daemon rewrites the record at next startup — nothing to do.
- **In a fleet, only the hub writes that record.** The domain has one A record
  and the hub is the one node that can serve every host's hostnames (it proxies
  each label to its owner). A member daemon with the same domain configured
  routes proxied requests but never touches DNS.
  - Ownership follows the `tag:ccmux-hub` tag, so make sure the hub node
    actually carries it — an untagged fleet has no agreed owner, and the daemon
    log says so on every reconcile.
  - A hub that is merely offline keeps the record. Handing it to a member would
    serve that one member's hostnames and break everyone else's, then flap when
    the hub returns. Untag or delete the node to move ownership.
  - The owner rewrites the record every 5 minutes, so a value something else
    stomped heals on its own — no restart needed.
- Each daemon with the domain configured issues its own copy of the wildcard
  cert, so keep the Cloudflare token off members that don't need to serve
  hostnames directly; hub-proxied requests use the member's own ts.net cert.
- Symptom of a stolen record: `ccmux devhost: no workspace maps <name>` with an
  empty "known hostnames" list, while the mappings are plainly still there in
  the app. Check `dig +short '*.<domain>'` against the hub's tailnet IP.
- CNAME to a ts.net name does NOT work and never will here: non-Funnel ts.net
  names don't exist in public DNS (verified 2026-07-16; see plan).

## Mode 2 — ts.net fallback (`chartlabs-app.tailb9053d.ts.net`)

Active whenever no custom domain is set. Each hostname becomes its own tailnet
node with an automatic ts.net cert.

- **Silent registration** needs a Tailscale auth key in Settings
  (login.tailscale.com → Settings → Keys → Auth key: **Reusable** and
  **Pre-approved**; tags optional). Without one, each new hostname logs an
  interactive join URL to `~/Library/Logs/ccmuxd.log` and waits.
- Auth keys expire (≤90 days). A new key only matters for NEW hostnames;
  registered nodes keep their identity in
  `~/Library/Application Support/ccmuxd/devhost/tsnet-hosts/<name>`.
- Each hostname shows up as a device in the Tailscale admin console; deleting
  a mapping removes its local identity, but the admin console may still list
  the device as offline until you delete it there.
- If the tailnet has device approval on, pre-approved keys skip it; otherwise
  approve each hostname node once in the admin console.
- Hostname labels land in the public CT log ledger (ts.net cert issuance) —
  don't encode anything sensitive in the name.

## Switching modes

Just set or clear the domain in Settings. Routing flips atomically; fallback
node identities are kept on disk when entering domain mode, so flipping back
re-adopts them without re-registration.

## Troubleshooting

- **Settings shows `error: …` for the cert**: token wrong/underscoped, or the
  domain isn't in the Cloudflare account. Fix and Save again (any settings
  save re-reconciles).
- **URL resolves but 502 "isn't answering"**: the mapping is fine, nothing
  listens on the port on the daemon host — check the workspace's dev server
  and that it binds 127.0.0.1 (not only a container-internal interface).
- **Name resolves to nothing off-tailnet**: expected — that's the design.
- **Who serves what**: `log stream`-free check —
  `curl -s http://127.0.0.1:7900/v1/settings | jq '{devDomain, devCertStatus}'`
  and `tail ~/Library/Logs/ccmuxd.log` for `devhost:` lines.
- Daemon restart after rebuild:
  `launchctl kickstart -k gui/501/com.ccmux.ccmuxd`.
