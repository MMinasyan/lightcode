PREFIX ?= $(HOME)/.local

# VERSION stays "dev" for source builds (an exact vX.Y.Z stamps a release
# build; used when testing upgrade flows). COMMIT is best-effort.
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null)
VERSION_PKG := github.com/MMinasyan/lightcode/internal/version
LDFLAGS := -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT)

build:
	~/go/bin/wails build -tags webkit2_41 -ldflags '$(LDFLAGS)'

test:
	go test ./...

test-race:
	go test -race ./...

test-integration:
	go test -race -tags webkit2_41 ./runtime ./internal/desktopclient

bench:
	go test -bench=. -benchmem ./...

PKG ?= ./model

fuzz-short:
	go test $(PKG) -fuzz=. -fuzztime=30s

# Lint rule classes intentionally excluded: G204 (subprocess), G304
# (file inclusion), G306/G302 (file perms), G301 (dir perms). Lightcode
# IS a file-IO + command-runner tool; these rules describe its design.
GOSEC_EXCLUDE := G204,G301,G302,G304,G306
GOBIN := $(shell go env GOBIN)
ifeq ($(strip $(GOBIN)),)
GOBIN := $(shell go env GOPATH)/bin
endif

install-lint-tools:
	go install github.com/securego/gosec/v2/cmd/gosec@v2.26.1
	go install github.com/gordonklaus/ineffassign@v0.2.0

lint: install-lint-tools
	$(GOBIN)/gosec -quiet -exclude=$(GOSEC_EXCLUDE) ./...
	$(GOBIN)/ineffassign ./...

install: build
	dest="$(PREFIX)/bin"; \
	mkdir -p "$$dest" || exit 1; \
	tmp=$$(mktemp "$$dest/.lightcode.tmp.XXXXXX") || exit 1; \
	trap 'rm -f "$$tmp"' EXIT HUP INT TERM; \
	cp build/bin/lightcode "$$tmp" || exit 1; \
	chmod 755 "$$tmp" || exit 1; \
	mv "$$tmp" "$$dest/lightcode" || exit 1

uninstall:
	rm -f $(PREFIX)/bin/lightcode

generate-protocol:
	go run ./internal/protocolgen -schema protocol/openapi.yaml -config protocol/oapi-codegen.yaml -sdk-out protocol/protocol.gen.go -out internal/clientops/operations.gen.go -native-contract-out frontend/src/generated/native-invocations.json
	cd frontend && npm run generate-protocol

check-protocol: generate-protocol
	sh scripts/check-generated-files.sh protocol/protocol.gen.go frontend/src/generated/protocol internal/clientops/operations.gen.go frontend/src/generated/native-invocations.json
	cd frontend && npm run typecheck:protocol

# Compares the Wails-generated Go bindings against the index. Prerequisite:
# the real Wails build (which runs the models.ts normalizer) has produced the
# tracked files under frontend/client-bindings/wailsjs/go.
check-wails-bindings:
	sh scripts/check-generated-files.sh frontend/client-bindings/wailsjs/go

.PHONY: build test test-race test-integration bench fuzz-short install-lint-tools lint install uninstall generate-protocol check-protocol check-wails-bindings
