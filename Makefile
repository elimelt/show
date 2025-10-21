# Make targets for 'show'. Author: Elijah Melton
SHELL := /bin/bash

APP := show
MAIN := ./cmd/show
BIN := bin/$(APP)
VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test install clean tidy

all: build

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(MAIN)

test:
	go test ./...

install: build
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 $(BIN) $(DESTDIR)$(PREFIX)/bin/$(APP)
	install -d $(DESTDIR)$(PREFIX)/share/man/man1
	install -m 0644 man/show.1 $(DESTDIR)$(PREFIX)/share/man/man1/show.1

tidy:
	go mod tidy

clean:
	rm -rf bin

