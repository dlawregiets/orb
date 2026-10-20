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

LDFLAGS := -s -w

.DEFAULT_GOAL := build

.PHONY: build install test lint fmt vet tidy clean run

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) $(CMD)

install:
	GOBIN=$(BINDIR) $(GO) install -trimpath -ldflags "$(LDFLAGS)" $(CMD)

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
