VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/guygrigsby/rudy/internal/cli.version=$(VERSION)

.PHONY: build test lint check fmt-check vendor-types install redeploy

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/rudy ./cmd/rudy

test:
	CGO_ENABLED=0 go test -race ./...

lint:
	golangci-lint run ./...

fmt-check:
	@out=$$(gofmt -l . 2>/dev/null); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# vendor-types is the grep gate on the three vendor SDKs: each stays inside the one
# package that owns it, so no wire type leaks into the kernel.
vendor-types:
	@bad=$$(grep -rl 'anthropic-sdk-go' internal | grep -v '^internal/provider/anthropicmsgs/'); test -z "$$bad" || { echo "anthropic sdk outside its codec:"; echo "$$bad"; exit 1; }
	@bad=$$(grep -rl 'modelcontextprotocol' internal | grep -v '^internal/plugins/mcp/'); test -z "$$bad" || { echo "mcp sdk outside its plugin:"; echo "$$bad"; exit 1; }
	@bad=$$(grep -rl 'memory-go' internal | grep -v '^internal/plugins/memory/'); test -z "$$bad" || { echo "memory-go outside its plugin:"; echo "$$bad"; exit 1; }

check: fmt-check vendor-types
	go vet ./...
	$(MAKE) lint
	$(MAKE) test

install:
	CGO_ENABLED=0 go install -ldflags "$(LDFLAGS)" ./cmd/rudy

redeploy: install
