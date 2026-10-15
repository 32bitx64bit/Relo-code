GO ?= go
CORE := core
EXT := extension
BIN := $(EXT)/bin
VERSION ?= 0.1.0

.PHONY: all build core core-all extension webview test lint package fmt dev install vsix preflight doctor

all: build

# One command: build for this machine, package, and install into VS Code.
dev install:
	sh scripts/install-dev.sh

# Build the .vsix only (no install). Used by CI.
vsix:
	RELO_SKIP_INSTALL=1 sh scripts/install-dev.sh

# Read-only environment checklist (never touches OpenCode processes).
preflight doctor:
	sh scripts/doctor.sh

build: core extension webview

core:
	mkdir -p $(BIN)
	cd $(CORE) && $(GO) build -ldflags "-s -w" -o ../$(BIN)/relo-core-$$(go env GOOS)-$$(go env GOARCH)$$(go env GOEXE) ./cmd/relo-core

core-all:
	mkdir -p $(BIN)
	cd $(CORE) && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags "-s -w" -o ../$(BIN)/relo-core-linux-amd64 ./cmd/relo-core
	cd $(CORE) && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -ldflags "-s -w" -o ../$(BIN)/relo-core-windows-amd64.exe ./cmd/relo-core
	cd $(CORE) && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -ldflags "-s -w" -o ../$(BIN)/relo-core-darwin-arm64 ./cmd/relo-core
	cd $(CORE) && CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -ldflags "-s -w" -o ../$(BIN)/relo-core-darwin-amd64 ./cmd/relo-core

$(EXT)/node_modules: $(EXT)/package-lock.json
	cd $(EXT) && npm ci
	@touch $(EXT)/node_modules

extension: $(EXT)/node_modules
	cd $(EXT) && npm run compile

webview:
	mkdir -p $(EXT)/media
	cp webview/src/main.js $(EXT)/media/webview.js

test:
	cd $(CORE) && $(GO) test ./...
	cd $(EXT) && npm test
	cd webview && npm test

lint:
	cd $(CORE) && $(GO) vet ./...
	cd $(CORE) && test -z "$$(gofmt -l .)"
	cd $(EXT) && npm run lint

fmt:
	cd $(CORE) && gofmt -w .

package: core-all extension webview
	@if test -x $(EXT)/node_modules/.bin/vsce; then cd $(EXT) && ./node_modules/.bin/vsce package --no-dependencies --allow-missing-repository; else echo "vsce missing; run 'cd extension && npm ci' first" >&2; exit 1; fi

clean:
	rm -rf $(BIN) $(EXT)/out $(EXT)/*.vsix
