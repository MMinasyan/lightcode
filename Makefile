PREFIX ?= ~/.local

build:
	~/go/bin/wails build

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

install: build
	mkdir -p $(PREFIX)/bin
	rm -f $(PREFIX)/bin/lightcode
	cp build/bin/lightcode $(PREFIX)/bin/lightcode

uninstall:
	rm -f $(PREFIX)/bin/lightcode

.PHONY: build test test-race test-integration bench fuzz-short install uninstall
