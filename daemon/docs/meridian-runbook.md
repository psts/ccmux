# Meridian runbook: a Claude subscription for opencode panes

Meridian is a community proxy that speaks the Anthropic Messages API and
forwards through the Claude Agent SDK, so a non-Claude-Code harness can spend
a Claude subscription. Anthropic refuses subscription tokens from third-party
callers server-side, which is why the daemon's own proxy cannot do this and
why the `claude` account kind is never offered to opencode. Background and
the 2026-09-05 spike: `agents-plan.md`.

## Install on a host

```sh
npm install -g --allow-scripts=@rynfar/meridian,@anthropic-ai/claude-code @rynfar/meridian
ln -sf "$(npm prefix -g)/bin/meridian" ~/.local/bin/meridian
meridian --version
```

The `--allow-scripts` list matters: the bundled Claude Code package fetches
its binary in a postinstall, and npm blocks that by default. The daemon looks
for `meridian` on PATH and in `~/.local/bin`, and it starts nothing until an
account asks for it, so installing after the daemon is running is fine.

## Add the account

Settings → Accounts → Add account:

- kind `meridian`
- base URL empty (defaults to `http://127.0.0.1:3456`; another loopback port
  is allowed, nothing else is)
- key: the output of `claude setup-token` for the subscription

Saving starts the sidecar. The account card shows `⚙ sidecar running (pid …)`
or the last error. The daemon restarts it with backoff if it exits and stops
it when the account is deleted. The token reaches Meridian only as
`CLAUDE_CODE_OAUTH_TOKEN` in its environment; the proxy never forwards it.

A meridian account cannot be the global route. It pairs per pane at harness
start: opencode's allowed kinds are meridian, anthropic, openai in that
order. If the global route already fits opencode (an anthropic or openai
account, Ollama for instance) the pane follows the global route; otherwise,
which is the case when the global route is a `claude` subscription account,
the pane is routed to the first meridian account. Either way the per-pane
route menu can override it. A shell pane and the claude harness never get a
meridian account offered: a Claude Code session riding the sidecar would be
one agent loop inside another.

## What opencode panes get

Every hosted pane carries `OPENCODE_CONFIG_CONTENT` pointing opencode's
anthropic provider at the pane's ccmux proxy, with snapshots off (they halve
prompt-cache hits whenever a new file appears) and the Meridian opencode
plugin when Meridian is installed. Nothing to configure in opencode itself.

## Check

```sh
curl -s http://127.0.0.1:3456/health | jq .auth,.mode,.plugin
curl -s http://127.0.0.1:3456/telemetry/summary | jq .totalRequests,.proxyOverhead
```

`/health` judges login from `~/.claude/.credentials.json`, which may be stale
even when the token in the account works; trust a real request (or the pane)
over that field.
