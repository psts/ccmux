#!/usr/bin/env bash
# Go cyclomatic-complexity gate for the daemon, capped at 10.
#
# RATCHET, not a wall. The daemon had 28 non-test functions over 10 when this
# gate was added, and qa-receipt cannot baseline a non-test gate the way
# /qa:check baselines a diff — it runs whole-repo commands. So the frozen list
# beside this script records that debt by identity, and the gate fails on:
#
#   - a function over the cap that is NOT in the list (new debt), or
#   - a listed function whose complexity has GROWN since it was frozen.
#
# Existing debt therefore ships, and cannot quietly get worse. Refactoring an
# entry below its frozen number always passes; prune it from the list when it
# drops under the cap.
#
# The list is DELETE-ONLY. It is a record of what was already there, not a
# hatch for new debt — a list you may append to is just a slower way of having
# no cap at all.
#
# Test files are excluded, matching /qa:check's complexity gate: table-driven
# tests and long integration setups trip the metric for organisational reasons,
# not real branching. 71 of the 99 violations here were tests.
#
# Identity is package + function + FILE, deliberately without the line number
# gocyclo prints — otherwise every edit above a frozen function would look like
# a new violation.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
FROZEN="$ROOT/.claude/gates/gocyclo-frozen.txt"

# go install puts it in GOPATH/bin, which is often not on PATH.
export PATH="$PATH:$(go env GOPATH 2>/dev/null)/bin"
if ! command -v gocyclo >/dev/null 2>&1; then
  echo "qa-gates: gocyclo not installed — this gate cannot run, so it fails rather"
  echo "than passing on no evidence. Install it with:"
  echo "  go install github.com/fzipp/gocyclo/cmd/gocyclo@latest"
  exit 1
fi

# complexity <TAB> pkg <TAB> func <TAB> file   (line:col stripped)
current=$(cd "$ROOT/daemon" && gocyclo -over 10 . 2>/dev/null \
  | grep -v '_test\.go' \
  | awk '{ split($4, a, ":"); print $1 "\t" $2 "\t" $3 "\t" a[1] }' \
  | sort -k2,4)

[ -z "$current" ] && { echo "qa-gates: gocyclo clean (no function over 10)"; exit 0; }

fail=0
while IFS=$'\t' read -r cx pkg fn file; do
  [ -z "${cx:-}" ] && continue
  frozen=$(awk -F'\t' -v p="$pkg" -v f="$fn" -v s="$file" \
    '$2==p && $3==f && $4==s { print $1 }' "$FROZEN" 2>/dev/null | head -1)
  if [ -z "$frozen" ]; then
    echo "NEW over-cap function (complexity $cx > 10): $pkg $fn  $file"
    fail=1
  elif [ "$cx" -gt "$frozen" ]; then
    echo "WORSE than frozen ($frozen -> $cx): $pkg $fn  $file"
    fail=1
  fi
done <<< "$current"

if [ "$fail" -ne 0 ]; then
  echo
  echo "Complexity cap is 10. Split the function, or land it under the cap."
  echo "The frozen list is DELETE-ONLY: adding a line to keep this gate quiet is"
  echo "how the list stops meaning anything. Prune entries as they drop under 10."
  exit 1
fi

echo "qa-gates: gocyclo ok ($(printf '%s\n' "$current" | wc -l) frozen violations, none new or worse)"
