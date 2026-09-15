VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/guygrigsby/rudy/internal/cli.version=$(VERSION)

.PHONY: build test lint check fmt-check vendor-types install redeploy config-example config-sync

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/rudy ./cmd/rudy

# The race detector needs cgo on Linux, so the tests build with it even though the binary
# does not: `build` stays CGO_ENABLED=0 and ships a static rudy.
test:
	CGO_ENABLED=1 go test -race ./...
	CGO_ENABLED=1 RUDY_TEST_TRANSPORT=socket go test -race -count=1 ./internal/server/

lint:
	golangci-lint run ./...

fmt-check:
	@out=$$(gofmt -l . 2>/dev/null); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# vendor-types is the grep gate on the four vendor SDKs: each stays inside the package
# that owns it, so no wire type leaks into the kernel.
vendor-types:
	@bad=$$(grep -rl 'anthropic-sdk-go' internal | grep -v '^internal/provider/anthropicmsgs/'); test -z "$$bad" || { echo "anthropic sdk outside its codec:"; echo "$$bad"; exit 1; }
	@bad=$$(grep -rl 'modelcontextprotocol' internal | grep -v '^internal/plugins/mcp/'); test -z "$$bad" || { echo "mcp sdk outside its plugin:"; echo "$$bad"; exit 1; }
	@bad=$$(grep -rl 'memory-go' internal | grep -v '^internal/plugins/memory/'); test -z "$$bad" || { echo "memory-go outside its plugin:"; echo "$$bad"; exit 1; }
	@bad=$$(grep -Erl --include='*.go' '^[[:space:]]*(import[[:space:]]+)?([._[:alnum:]]+[[:space:]]+)?"github.com/coder/acp-go-sdk([/"]|$$)' . | grep -v '^./internal/acpagent/' | grep -v '^./internal/acpclient/'); test -z "$$bad" || { echo "acp sdk outside its adapters:"; echo "$$bad"; exit 1; }

check: fmt-check vendor-types
	go vet ./...
	$(MAKE) lint
	$(MAKE) test

install:
	CGO_ENABLED=0 go install -ldflags "$(LDFLAGS)" ./cmd/rudy
	$(MAKE) config-sync

# config-sync adds keys the operator's config.toml is missing, with the comment that says
# what each is for. It never changes a value that is already there, and it writes a whole
# commented file when there is none. Part of install so a new key reaches the file it
# belongs in rather than living only in the defaults.
config-sync: build
	./bin/rudy config sync

# config-example regenerates the example this repository ships from the defaults. The
# drift test in internal/config fails until it is run.
config-example: build
	./bin/rudy config example > examples/config.toml

redeploy: install
