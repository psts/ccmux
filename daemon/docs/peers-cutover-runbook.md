# Peers bus cutover — from claude-peers-mcp to ccmuxd

The peers messaging network now lives inside ccmuxd (`daemon/internal/peers/`)
with a per-session thin MCP client (`daemon/cmd/ccmux-peers/`). Grouping is by
**ccmux window** (workspace `Group`) with the old parent-directory fallback for
sessions outside ccmux. Everything is built, unit-tested, race-clean, and
verified end-to-end against an isolated daemon (window scoping, cross-window
guard, channel push through the real thin client, the full permission-relay
round trip, live window moves, and the web viewer on desktop + phone widths).

What was deliberately NOT touched by the agent (running sessions depend on it):
the live broker on `:7899`, the user-scope MCP entry pointing at bun
`server.ts`, and the production ccmuxd. This runbook is that cutover.

---

## 0. What changes for a session

Nothing visible: tool names/schemas, the `<channel source="claude-peers">` tag,
relay wording, and the `--dangerously-load-development-channels
server:claude-peers` launch flag are identical. What improves underneath:

- **Groups = ccmux windows.** Panes in the same window are peers; renaming or
  moving a workspace re-groups its sessions instantly (resolution is live).
  Plain-terminal sessions keep today's `dirname(git_root)` grouping.
- **No duplicate/lost messages.** Event log + per-peer cursor replaced the old
  delivered-flag (WS reconnects used to re-inject everything unpolled).
- **Messages persist 30 days** (SQLite, same registry DB) instead of 3h in
  broker memory.
- **Authenticated sends.** The old broker accepted any local (or blind no-cors
  browser) POST as any `from_id`. Now every mutation needs a bearer token —
  hosted panes get `CCMUX_PANE_TOKEN` in their env; plain terminals read
  `~/Library/Application Support/ccmuxd/peers.json` (0600) automatically.
- **`spawn_if_missing`** spawns natively into a live workspace in the sender's
  window when one hosts the repo (birth prompt fires exactly once, never on
  revive); otherwise the old `ccmux://spawn` deep link still fires.
- Web/phone lens: every named group header has a 💬 button → read-only message
  history + live stream.

## 1. Ship order (one sitting, ~5 min)

Bus and Mac overlay move together — the app now reads `/v1/peers/*` from the
daemon, so run the new daemon before (or with) the new app.

```sh
cd ~/Work/Coding/ccmux/daemon
go build -o ~/bin/ccmuxd ./cmd/ccmuxd          # or wherever ccmuxd lives
go build -o ~/bin/ccmux-peers ./cmd/ccmux-peers
# restart ccmuxd however it's supervised (hosted tmux sessions survive; the
# daemon re-adopts them on start). First start logs:
#   peers bus enabled (pane-less info .../ccmuxd/peers.json)
./build-app.sh                                  # rebuild + relaunch the Mac app
```

## 2. Swap the MCP server (new sessions only)

```sh
claude mcp remove --scope user claude-peers
claude mcp add --scope user --transport stdio claude-peers -- ~/bin/ccmux-peers
```

Running sessions keep their old bun server + broker until they recycle — the
two stacks coexist (different ports), so there is no flag day. The launch
alias/flag is unchanged.

Existing hosted panes created by an OLD daemon have no `CCMUX_PANE_TOKEN` in
their pane env — sessions there come up **pane-less** (dirname grouping) until
the pane is recreated (new pane, revive, or workspace re-create under the new
daemon). New panes get window grouping immediately.

## 3. Decommission the old broker (when no old session remains)

```sh
cd ~/Work/Coding/claude-peers-mcp && bun cli.ts kill-broker
```

Nothing relaunches it once no session runs the old `server.ts`. The repo stays
as reference; `CLAUDE_PEERS_PORT` and the `:7899` UI are dead.

## 4. Verify

1. Two sessions in one window: `list_peers` shows only each other
   (`project: <WINDOW NAME>`); a session in another window is invisible and
   unreachable ("Cannot send messages across projects").
2. Send between them → `<channel>` tag arrives once; `check_messages`
   afterwards says "No new messages" (shared cursor — this was broken before).
3. Delegate work A→B, let B hit a permission prompt → A gets the relay,
   replies `yes <id>` → B's dialog resolves; the verdict text never shows as
   chat in B.
4. Web lens → group header 💬 → history + live updates, read-only.
5. Rename the window → next `list_peers` shows the new group name, no restarts.

## Notes / limitations

- Mac-local (legacy driver-mode) panes have no daemon pane record → their
  sessions group by dirname fallback, not the window. Hosted panes are the
  window-grouped path. (Follow-up if it matters: inject a group env in
  `TerminalStore`.)
- Deep-link-spawned teammates (Mac ephemeral split panes) are pane-less; the
  daemon pins them into the requester's group when they register, so the
  conversation works — but the messages viewer for a *window* shows them under
  the requester's group, which is what you want.
- The permission relay's trust boundary is unchanged: mutations are
  loopback-only + token-authed; the viewer surface (read-only) is what the
  tailnet sees. Phone-approve would need a write-enabled authed endpoint —
  the structured-verdict design already supports it, deliberately not built.
- Auto-summaries (the old OpenAI `gpt-5.4-nano` startup summary) were dropped in
  the port; `set_summary` is the path. Trivial to re-add daemon-side if missed.
