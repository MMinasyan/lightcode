PREFIX ?= ~/.local

build:
	~/go/bin/wails build

install: build
	mkdir -p $(PREFIX)/bin
	rm -f $(PREFIX)/bin/lightcode
	cp build/bin/lightcode $(PREFIX)/bin/lightcode

uninstall:
	rm -f $(PREFIX)/bin/lightcode

.PHONY: build install uninstall
