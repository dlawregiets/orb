GO ?= go
BINARY := orb
CMD := ./cmd/orb
BUILD_DIR := build

# Where `make install` puts the binary. Defaults to the Go bin directory,
# which is $GOBIN or $GOPATH/bin. Override with `make install BINDIR=/usr/local/bin`.
BINDIR ?= $(shell $(GO) env GOBIN)
ifeq ($(strip $(BINDIR)),)
BINDIR := $(shell $(GO) env GOPATH)/bin
endif

# Release builds set VERSION in the environment; local builds derive it from the
# nearest git tag, stripped of its leading "v".
VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo dev))
ifeq ($(strip $(VERSION)),)
VERSION := dev
endif

LDFLAGS := -s -w -X main.version=$(VERSION)

# Releases are distributed as bare tarballs, so the binary must not depend on the
# host's libc. Nothing here uses cgo; override with `make build CGO_ENABLED=1` if
# that ever changes.
CGO_ENABLED := 0

.DEFAULT_GOAL := build

.PHONY: build install test lint fmt vet tidy clean run

build:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) $(CMD)

install:
	GOBIN=$(BINDIR) CGO_ENABLED=$(CGO_ENABLED) $(GO) install -trimpath -ldflags "$(LDFLAGS)" $(CMD)

test:
	$(GO) test ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w $(shell find . -name '*.go' -not -path './build/*')

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

run:
	$(GO) run $(CMD) $(ARGS)

clean:
	rm -rf $(BUILD_DIR)
