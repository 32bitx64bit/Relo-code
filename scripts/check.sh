#!/bin/sh
# Local equivalent of .github/workflows/ci.yml
set -eu

cd "$(dirname "$0")/.."

(
	cd core
	go vet ./...
	test -z "$(gofmt -l .)" || { gofmt -l .; echo "gofmt: files need formatting" >&2; exit 1; }
	go test -race ./...
)

(
	cd extension
	npm ci
	npm run compile
	npm run lint
	npm test
)
