SHELL := /bin/bash
APP := show
MAIN := ./cmd/show
OUT := bin/$(APP)
VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help
help:
	@echo "Targets: build, test, run, clean, release, snapshot, install"

.PHONY: build
build:
	@mkdir -p bin
	@echo "Building $(APP) $(VERSION) -> $(OUT)"
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT) $(MAIN)

.PHONY: test
test:
	go test ./...

.PHONY: run
run: build
	./$(OUT) -version

.PHONY: clean
clean:
	rm -rf bin dist

.PHONY: snapshot
snapshot:
	@goreleaser release --snapshot --clean

.PHONY: release
release:
	@goreleaser release --clean

.PHONY: install
install: build
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 $(OUT) $(DESTDIR)$(PREFIX)/bin/$(APP)
	install -d $(DESTDIR)$(PREFIX)/share/man/man1
	install -m 0644 man/show.1 $(DESTDIR)$(PREFIX)/share/man/man1/show.1

