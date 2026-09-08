VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/guygrigsby/rudy/internal/cli.version=$(VERSION)

.PHONY: build test lint check fmt-check install redeploy

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/rudy ./cmd/rudy

test:
	CGO_ENABLED=0 go test -race ./...

lint:
	golangci-lint run ./...

fmt-check:
	@out=$$(gofmt -l . 2>/dev/null); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check: fmt-check
	go vet ./...
	$(MAKE) lint
	$(MAKE) test

install:
	CGO_ENABLED=0 go install -ldflags "$(LDFLAGS)" ./cmd/rudy

redeploy: install
