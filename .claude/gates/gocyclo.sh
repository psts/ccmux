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

# Capture the run, its status and its stderr BEFORE filtering. The old pipeline
# sent stderr to /dev/null and piped straight into grep/awk, so a gocyclo that
# could not run left $current empty and the emptiness check below printed
# "clean" and exited 0 — passing on no evidence, the exact thing the
# missing-binary branch above refuses to do.
#
# The exit code alone cannot fix that. Measured 2026-08-30, all three cases:
#
#   found functions over the cap : rc=1, stdout 99 lines, stderr EMPTY
#   nothing over the cap         : rc=0, stdout empty,    stderr EMPTY
#   a file that does not parse   : rc=1, stdout EMPTY,    stderr set
#
# rc=1 is therefore the NORMAL case here (28 functions are frozen), and keying
# the failure on it fails every run. stderr is the signal that separates them:
# gocyclo is silent on it whenever it works, and it aborts outright rather than
# reporting partial results, so anything on stderr means the cap went unchecked.
errfile=$(mktemp)
raw=$(cd "$ROOT/daemon" && gocyclo -over 10 . 2>"$errfile")
rc=$?
errs=$(cat "$errfile")
rm -f "$errfile"

if [ -n "$errs" ]; then
  echo "qa-gates: gocyclo could not read some files, so the cap was never checked:"
  printf '%s\n' "$errs"
  exit 1
fi

if [ "$rc" -ne 0 ] && [ -z "$raw" ]; then
  echo "qa-gates: gocyclo exited $rc with no output and no error — this gate"
  echo "cannot report, so it fails rather than passing on no evidence."
  exit 1
fi

# The empty case has to short-circuit BEFORE the pipeline. `printf '%s\n' ""`
# emits a blank line, grep keeps it (it holds no "_test.go"), and awk turns it
# into "\t\t\t" — a 3-byte record that is not empty. Every emptiness check
# below would then be unreachable in exactly the case they exist for, the loop
# would run once on a blank whose awk lookup matches the frozen file's untabbed
# comment lines, and the gate would print "1 over-cap functions" and exit 0.
# Measured with a gocyclo shim exiting 0 on empty output: green, on nothing.
if [ -z "$raw" ]; then
  current=""
else
  # complexity <TAB> pkg <TAB> func <TAB> file   (line:col stripped)
  current=$(printf '%s\n' "$raw" \
    | grep -v '_test\.go' \
    | awk '{ split($4, a, ":"); print $1 "\t" $2 "\t" $3 "\t" a[1] }' \
    | sort -k2,4)
fi

# The list has to exist before the loop reads it. Checking it per-function with
# awk's own error suppressed reported a missing file as every known violation
# being brand new, which sends the reader hunting for complexity nobody added.
[ -r "$FROZEN" ] || {
  echo "qa-gates: frozen list missing or unreadable: $FROZEN"
  echo "Without it every known violation reads as new. Restore it from git."
  exit 1
}
frozen_count=$(grep -v '^[[:space:]]*#' "$FROZEN" | grep -c '[^[:space:]]')

# A clean run and a run that produced nothing look identical from here, so cross
# -check against the baseline: with N functions frozen, zero output means the
# gate did not really run, whatever its exit code said.
if [ -z "$current" ] && [ "$frozen_count" -gt 0 ]; then
  echo "qa-gates: gocyclo produced no output, but $FROZEN lists $frozen_count known"
  echo "violations. The gate did not really run. Refusing to pass on that."
  exit 1
fi

[ -z "$current" ] && { echo "qa-gates: gocyclo clean (no function over 10)"; exit 0; }

fail=0
while IFS=$'\t' read -r cx pkg fn file; do
  # head -1 is load-bearing: a duplicated entry would make $frozen two lines and
  # the numeric -gt below would error out, silently reading as false.
  # !/^#/ so the lookup can never match a header line. Those carry no tabs, so
  # $2/$3/$4 are all empty on them and a record with empty fields would "find"
  # the comment text as its frozen complexity.
  frozen=$(awk -F'\t' -v p="$pkg" -v f="$fn" -v s="$file" \
    '!/^#/ && $2==p && $3==f && $4==s { print $1 }' "$FROZEN" | head -1)
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

# Counts $current, so say what $current is: functions over the cap right now. It
# is not the size of the frozen list, which stays 28 until an entry is pruned.
echo "qa-gates: gocyclo ok ($(printf '%s\n' "$current" | wc -l) over-cap functions, all frozen, none new or worse)"
