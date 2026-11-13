#!/usr/bin/env bash
# The full check set (AGENTS.md §6): format, vet, tests, build. Stops at the first failure.
set -euo pipefail
cd "$(dirname "$0")/.."

unformatted=$(gofmt -l .)
if [[ -n $unformatted ]]; then
	echo "gofmt: not formatted:" >&2
	echo "$unformatted" >&2
	exit 1
fi
echo "gofmt: ok"
go vet ./...
echo "vet: ok"
go test -race ./... 2>&1 | tail -n 20
go build -o bin/letsgoagain ./cmd/letsgoagain
echo "build: ok (bin/letsgoagain)"
