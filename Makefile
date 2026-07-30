VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS = -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

.PHONY: build test vet clean

# Real builds happen in CI (linux/amd64 and linux/arm64); this target is for
# a local sanity check.
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gora ./cmd/gora

# The race detector needs cgo, which the development machine does not have;
# CI runs `go test -race ./...` on Linux.
test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin dist
