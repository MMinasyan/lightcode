PREFIX ?= ~/.local

# VERSION stays "dev" for source builds (an exact vX.Y.Z stamps a release
# build; used when testing upgrade flows). COMMIT is best-effort.
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null)
VERSION_PKG := github.com/MMinasyan/lightcode/internal/version
LDFLAGS := -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT)

build:
	~/go/bin/wails build -ldflags '$(LDFLAGS)'

test:
	go test ./...

test-race:
	go test -race ./...

test-integration:
	go test -tags=integration -race ./...

bench:
	go test -bench=. -benchmem ./...

PKG ?= ./internal/provider

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
	mkdir -p $(PREFIX)/bin
	rm -f $(PREFIX)/bin/lightcode
	cp build/bin/lightcode $(PREFIX)/bin/lightcode

uninstall:
	rm -f $(PREFIX)/bin/lightcode

.PHONY: build test test-race test-integration bench fuzz-short install-lint-tools lint install uninstall
