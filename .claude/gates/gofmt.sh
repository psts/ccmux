#!/usr/bin/env bash
# gofmt gate for the daemon.
#
# `gofmt -l` lists unformatted files on stdout, so the emptiness of that output
# is HALF the result. The other half is the exit code, and it is easy to miss:
# on a file that does not PARSE, gofmt writes the error to stderr and exits 2
# with EMPTY stdout. The one-liner this replaced was
#
#   cd daemon && test -z "$(gofmt -l .)" || { ...; exit 1; }
#
# which therefore PASSED on unparseable Go (measured 2026-08-30: exit 2, stdout
# empty), and carried a comment claiming gofmt "exits 0 either way". `go vet`
# further down qa-gates still catches such a file, so the receipt as a whole
# never went green on broken Go — but a gate that cannot run must fail, not
# report success on no evidence.
#
# It is a script rather than a qa-gates line because qa-gates is one command per
# line: there is nowhere in it to put a status check without cramming.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

if ! command -v gofmt >/dev/null 2>&1; then
  echo "qa-gates: gofmt not installed — this gate cannot run, so it fails rather"
  echo "than passing on no evidence. Install the Go toolchain."
  exit 1
fi

cd "$ROOT/daemon" || { echo "qa-gates: gofmt — $ROOT/daemon not found"; exit 1; }

# Keep the two streams apart rather than folding stderr into stdout: $out must
# hold filenames and nothing else, or a gofmt that ever warned while exiting 0
# would be reported as an unformatted file named after the warning. The status
# is what the bare command substitution used to discard.
errfile=$(mktemp)
out=$(gofmt -l . 2>"$errfile")
rc=$?
errs=$(cat "$errfile")
rm -f "$errfile"

if [ "$rc" -ne 0 ] || [ -n "$errs" ]; then
  echo "qa-gates: gofmt could not check formatting (exit $rc), so it fails rather"
  echo "than passing on no evidence:"
  [ -n "$errs" ] && printf '%s\n' "$errs"
  exit 1
fi

if [ -n "$out" ]; then
  echo "qa-gates: gofmt — not formatted:"
  printf '%s\n' "$out"
  exit 1
fi

echo "qa-gates: gofmt ok"
