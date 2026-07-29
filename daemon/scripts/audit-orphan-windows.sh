#!/usr/bin/env bash
# audit-orphan-windows.sh — list every live window in the ccmux tmux server with
# its last-activity time, oldest first.
#
# Why this exists: ccmux maps a workspace to a tmux session and a pane to a tmux
# window. When the daemon restarts it re-adopts the existing tmux sessions, but a
# window that isn't rendered as a tab in any lens (not referenced by the
# workspace's saved layout) stays alive here — invisible yet running (its Claude
# still registers on the peers bus). Those accumulate silently; the old ones stand
# out by age in this listing.
#
# This is a read-only audit. Reap an orphan yourself once you've confirmed it:
#   tmux -L "$SOCKET" kill-window -t <session>:<index>
# The daemon reaps the pane on pane-exited, which unregisters its peer from the bus.
#
# The systemic fix (surface untracked live windows as tabs, or a "close orphaned
# panes" action) is a daemon follow-up — see daemon/docs/multihost-plan.md notes.
set -euo pipefail

socket="${CCMUX_TMUX_SOCKET:-ccmux}"

if ! tmux -L "$socket" list-sessions >/dev/null 2>&1; then
	echo "no ccmux tmux server on socket -L $socket (nothing to audit)"
	exit 0
fi

printf '%-30s %-8s %-18s %s\n' 'SESSION:WIN' 'WINDOW' 'LAST-ACTIVITY' 'NAME'
tmux -L "$socket" list-windows -a \
	-F '#{window_activity}	#{session_name}:#{window_index}	#{window_id}	#{window_name}' |
	sort -n |
	while IFS=$'\t' read -r epoch loc wid name; do
		if [ -n "$epoch" ] && [ "$epoch" -gt 0 ] 2>/dev/null; then
			when=$(date -r "$epoch" '+%Y-%m-%d %H:%M' 2>/dev/null || echo "$epoch")
		else
			when='(unknown)'
		fi
		printf '%-30s %-8s %-18s %s\n' "$loc" "$wid" "$when" "$name"
	done
