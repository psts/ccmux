#!/bin/bash
# Prints the hash of the LLM proxy service's own code: the one value that
# decides whether a deploy restarts ccmux-llm-proxy (build-daemon.sh stamps it
# into the binary; internal/llmserve/hash.go reports it; the daemon restarts
# the service when the running one reports another).
#
# What goes in:
#   - the Go version;
#   - every package of THIS module that internal/llmserve imports, itself
#     included: each non-test Go file and embedded file, by content;
#   - every other module it imports, by path and version (go.sum pins what a
#     version holds, so the content need not be read).
#
# What stays out, on purpose: the checkout's location (files are named by
# import path, so a deploy from another folder or worktree gets the same
# hash), test files, and everything else in the ccmuxd binary. The proxy is
# the same program as the daemon; only this slice of it runs in the service.
set -euo pipefail
cd "$(dirname "$0")/.."

sha() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum | cut -d' ' -f1
	else
		shasum -a 256 | cut -d' ' -f1
	fi
}

# One line per input: "F <import path>/<file> <absolute path>" for this
# module's files, "M <module>@<version>" for the others.
LIST=$(go list -deps -f '{{if and (not .Standard) .Module}}{{if .Module.Main}}{{$p := .ImportPath}}{{$d := .Dir}}{{range .GoFiles}}F {{$p}}/{{.}} {{$d}}/{{.}}
{{end}}{{range .EmbedFiles}}F {{$p}}/{{.}} {{$d}}/{{.}}
{{end}}{{else}}M {{.Module.Path}}@{{.Module.Version}}
{{end}}{{end}}' ./internal/llmserve)

{
	go env GOVERSION
	printf '%s\n' "$LIST" | grep '^M ' | sort -u
	printf '%s\n' "$LIST" | grep '^F ' | sort | while read -r _ name path; do
		printf '%s %s\n' "$name" "$(sha <"$path")"
	done
} | sha | cut -c1-16
