# Phase 6 — iPhone PWA install + on-device push test

The full web-push pipeline is built, committed, and verified agent-side (VAPID,
subscription CRUD, SW registration, manifest, and focus-aware suppression — the
last proven end-to-end against a live capture endpoint). The one thing that
needs a real device is **physical push delivery**. This runbook covers it.

Origin: `https://ccmuxd.tailb9053d.ts.net` · tailnet `sandelin@gmail.com`.

The daemon now serves as **its own tailnet node** (`ccmuxd`) via tsnet — no
`tailscale serve`, no Caddy contention, no `:443` collision (the node has its own
IP + `:443`). The `tailscale serve` + `mbp`-origin setup this runbook first
described is superseded.

---

## 0. What's already set up (agent-side)

- **Daemon** serves the push API + PWA. VAPID keypair is generated and persisted
  on first run at `~/Library/Application Support/ccmuxd/vapid.json` (0600).
- **tsnet node**: with `-tsnet` the daemon comes up as `ccmuxd.<tailnet>.ts.net`,
  terminating TLS on its own `:443` with a tailnet-issued cert and resolving
  caller identity in-process via WhoIs. Verified live (S5): node up on its own IP,
  HTTPS with a valid cert, WhoIs returns the connecting peer's tailnet login.
- The push API + PWA (`/v1/health`, `/v1/push/vapid`, `/manifest.webmanifest`,
  `/sw.js`, icons, secure-context SW registration) were all verified over HTTPS.

Caddy is **irrelevant now** — the tsnet node never touches the host's `:443`, so
Caddy can run its dev domains undisturbed. Nothing to stop or reconfigure.

---

## 1. Run the daemon as the tsnet node

The agent's daemon dies when its session ends, so start your own. First run needs
the node authorized — set your reusable `TS_AUTHKEY` (generate one at
`login.tailscale.com/admin/settings/keys`); after that, node state persists in
`~/Library/Application Support/ccmuxd/tsnet/` and the key isn't needed again.

```sh
cd ~/Work/Coding/ccmux/daemon
go build -o /tmp/ccmuxd ./cmd/ccmuxd
export TS_AUTHKEY=tskey-...              # only needed on first run / fresh state
/tmp/ccmuxd -tsnet -addr 127.0.0.1:7900  # tsnet node + loopback (hooks); launchd for reboot-survival
```

The `-addr` loopback listener still runs for on-host hooks and health; lenses use
the ts.net origin. For the **push test to fire from real activity**, the daemon
must own the hooks socket `/tmp/ccmux-hooks.sock` — the local ccmux *app* also
binds it and, if launched after the daemon, steals it (known deferred bug). So
either quit the app during the test, or use the synthetic hook in step 4b.

---

## 2. Install the PWA on the iPhone

1. iPhone on the **same tailnet**, iOS **16.4+**, **Tailscale on** (green, connected).
2. Open **Safari** → `https://ccmuxd.tailb9053d.ts.net/`. The session list loads.
3. **Share** (□↑) → **Add to Home Screen** → Add.
4. **Open ccmux from the new Home-Screen icon** (not Safari — push only works from
   the standalone install).

## 3. Enable notifications

1. In the installed PWA, tap the **⚙ gear** (top-left, next to +).
2. The sheet reads "Get a push when a session needs you." Tap **Enable
   notifications** → **Allow** at the iOS prompt.
   - If you opened it in Safari instead of the Home-Screen app, the sheet instead
     shows the "Add to Home Screen" instruction — that's expected; install first.
3. The sheet flips to "Notifications are on for this device." (This POSTs the
   subscription to the daemon, keyed to your verified tailnet login.)

## 4. Trigger a push

Make sure you're **not** actively watching the target workspace on the phone
(lock the phone or switch to another app — the daemon suppresses pushes while a
lens is attached **and** focused, and delivers them otherwise).

**4a. From real activity:** in a hosted workspace, let a Claude Code session reach
a prompt (permission request / idle) so it emits a `needs_input` hook.

**4b. Synthetic (deterministic) — fire a hook by hand.** Get a live pane id, then
write one hook message to the socket:

```sh
PANE=$(curl -s http://127.0.0.1:7900/v1/workspaces | python3 -c \
  "import json,sys; print(json.load(sys.stdin)[0]['panes'][0]['id'])")
python3 - "$PANE" <<'PY'
import socket, sys
m = '{"type":"permission_request","pane_id":"%s","cwd":"/tmp"}' % sys.argv[1]
s = socket.socket(socket.AF_UNIX); s.connect('/tmp/ccmux-hooks.sock')
s.sendall(m.encode()); s.close()
PY
```

## 5. Expected result

- A **notification** arrives on the locked phone: title = workspace name, body =
  "needs your input". (Same workspace re-firing **replaces** the notification
  rather than stacking — `tag` = workspace id.)
- **Tap it** → the PWA opens (or focuses) and **deep-links straight to that
  workspace** (`/?ws=<id>` → auto-attach).
- Open the workspace and keep it foregrounded, fire the hook again → **no push**
  (suppressed because you're now watching). Background the app, fire again → push
  returns. That's the suppression contract.

---

## Troubleshooting

| Symptom | Fix |
|---|---|
| ts.net URL won't load / TLS error | Node not up: check the daemon started with `-tsnet` and (first run) a valid `TS_AUTHKEY`; first HTTPS hit provisions the cert (retry after a few seconds). |
| Sheet stuck on "Add to Home Screen" | You're in Safari, not the installed app. Open from the Home-Screen icon. |
| "Notifications are blocked" | You tapped Don't Allow once. iOS Settings → ccmux → Notifications → Allow (or remove + re-add the PWA). |
| Enabled but no push arrives | Confirm you're not attached+focused on that workspace; confirm the daemon (not the app) owns `/tmp/ccmux-hooks.sock` (`lsof /tmp/ccmux-hooks.sock`); check the daemon log for the send. |
| Push worked once, then stopped | The subscription may have been pruned (the push service returned 404/410). Toggle notifications off/on in the ⚙ sheet to re-subscribe. |

## Quick reference

```sh
tailscale status | grep ccmuxd                        # the daemon's own node should be listed
curl -s https://ccmuxd.tailb9053d.ts.net/v1/push/vapid   # VAPID public key over the node's HTTPS
curl -s http://127.0.0.1:7900/v1/push/subscriptions?user=<you>   # your registered devices
```
