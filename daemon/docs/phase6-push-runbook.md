# Phase 6 — iPhone PWA install + on-device push test

The full web-push pipeline is built, committed, and verified agent-side (VAPID,
subscription CRUD, SW registration, manifest, and focus-aware suppression — the
last proven end-to-end against a live capture endpoint). The one thing that
needs a real device is **physical push delivery**. This runbook covers it.

Host: `mbp.tailb9053d.ts.net` · tailnet `sandelin@gmail.com` · daemon `:7900`.

---

## 0. What's already set up (agent-side)

- **Daemon** serves the push API + PWA. VAPID keypair is generated and persisted
  on first run at `~/Library/Application Support/ccmuxd/vapid.json` (0600).
- **`tailscale serve`** fronts the daemon: `https://mbp.tailb9053d.ts.net/` →
  `http://127.0.0.1:7900`. TLS cert issued (`tailscale cert` succeeds).
- Verified over the real HTTPS origin: `/v1/health`, `/v1/push/vapid`,
  `/manifest.webmanifest` (`application/manifest+json`), `/sw.js`, icons, and a
  secure-context service-worker registration all return 200.

### ⚠️ Caddy coexistence

Caddy owns `:443` (wildcard) for your `*.dev.chartlabs.io` dev domains and has no
route for the ts.net name, so **while Caddy runs it shadows `tailscale serve` and
the ts.net origin breaks**. You stopped Caddy for this test, which is why it works
now. Pick one before relying on it long-term:

- **Keep them separate** — run Caddy bound to specific interface IPs (not `:443`
  wildcard) so the tailnet IP is free for serve. Cleanest; needs a Caddy config edit.
- **Front through Caddy** — add a Caddy site for `mbp.tailb9053d.ts.net` →
  `127.0.0.1:7900` using `tls { get_certificate tailscale }` (your Caddy build has
  the module) and drop the serve config. Then Caddy handles both.
- **Just keep Caddy stopped** during phone use (fine for a one-off test).

Reset serve if you ever want it gone: `tailscale serve --https=443 off`.

---

## 1. Run the daemon persistently on :7900

The agent's daemon dies when its session ends, so start your own:

```sh
cd ~/Work/Coding/ccmux/daemon
go build -o /tmp/ccmuxd ./cmd/ccmuxd
/tmp/ccmuxd -addr 127.0.0.1:7900          # foreground; or a launchd LaunchAgent for reboot-survival
```

For the **push test to fire from real activity**, the daemon must own the hooks
socket `/tmp/ccmux-hooks.sock`. The local ccmux *app* also binds it and, if
launched after the daemon, steals it (known deferred bug). So either quit the app
during the test, or use the synthetic hook in step 4b.

---

## 2. Install the PWA on the iPhone

1. iPhone on the **same tailnet**, iOS **16.4+**, **Tailscale on** (green, connected).
2. Open **Safari** → `https://mbp.tailb9053d.ts.net/`. The session list loads.
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
| ts.net URL won't load / TLS error | Caddy is running and shadowing `:443` — see §0. |
| Sheet stuck on "Add to Home Screen" | You're in Safari, not the installed app. Open from the Home-Screen icon. |
| "Notifications are blocked" | You tapped Don't Allow once. iOS Settings → ccmux → Notifications → Allow (or remove + re-add the PWA). |
| Enabled but no push arrives | Confirm you're not attached+focused on that workspace; confirm the daemon (not the app) owns `/tmp/ccmux-hooks.sock` (`lsof /tmp/ccmux-hooks.sock`); check the daemon log for the send. |
| Push worked once, then stopped | The subscription may have been pruned (the push service returned 404/410). Toggle notifications off/on in the ⚙ sheet to re-subscribe. |

## Quick reference

```sh
tailscale serve status                       # should show / → http://127.0.0.1:7900
curl -s https://mbp.tailb9053d.ts.net/v1/push/vapid   # VAPID public key (needs Caddy stopped)
curl -s http://127.0.0.1:7900/v1/push/subscriptions?user=<you>   # your registered devices
```
