VERSION ?= $(shell git describe --tags --always --dirty=-dev 2>/dev/null || echo unknown)
LDFLAGS := -X github.com/guygrigsby/rudy/internal/cli.version=$(VERSION)

.PHONY: build test lint check fmt-check vendor-types vuln install redeploy config-example config-sync changelog release-notes release-check

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

# vendor-types is the grep gate on the three vendor SDKs: each stays inside the one
# package that owns it, so no wire type leaks into the kernel.
vendor-types:
	@bad=$$(grep -rl 'anthropic-sdk-go' internal | grep -v '^internal/provider/anthropicmsgs/'); test -z "$$bad" || { echo "anthropic sdk outside its codec:"; echo "$$bad"; exit 1; }
	@bad=$$(grep -rl 'modelcontextprotocol' internal | grep -v '^internal/plugins/mcp/'); test -z "$$bad" || { echo "mcp sdk outside its plugin:"; echo "$$bad"; exit 1; }
	@bad=$$(grep -rl 'memory-go' internal | grep -v '^internal/plugins/memory/'); test -z "$$bad" || { echo "memory-go outside its plugin:"; echo "$$bad"; exit 1; }

# vuln is its own target rather than a step in check: it asks the vulnerability database
# over the network, and the gate is worth more when it runs the same offline as on. CI runs
# it on every push and again weekly, because an advisory lands against code that did not
# change and no push announces it.
vuln:
	govulncheck ./...

check: fmt-check vendor-types
	go vet ./...
	$(MAKE) lint
	$(MAKE) test

# changelog writes the newest section of CHANGELOG.md from the commits since the last
# release, through rudy itself: the harness is the thing that reads a diff and says what
# changed, so the release notes are not a thing it makes a person do by hand. Read what it
# wrote before committing it; a model writing your release notes is a draft, not an author.
changelog:
	@$(MAKE) --no-print-directory build VERSION=
	@VERSION="$(VERSION)" DRY_RUN="$(DRY_RUN)" ./scripts/changelog.sh

# release-notes prints the newest release's section of CHANGELOG.md: everything under the
# first "## " heading down to the next one, with the leading blank lines dropped. The GitHub
# release carries this, so what a person reads there is what the binary's own startup header
# shows them (ADR 0016), rather than a list of commit subjects.
release-notes:
	@awk '/^## /{ if (seen) exit; seen=1; next } seen { print }' CHANGELOG.md | awk 'NF || seen { seen=1; print }'

# release-check refuses a tag the changelog does not know about. The tag builds a tree, and
# that tree's CHANGELOG.md is embedded in the binary, so a v0.2.0 tag over a changelog whose
# newest release is 0.1.0 ships a header advertising the release before it. VERSION is the
# tag, passed in by the release workflow and defaulted from git describe here.
release-check:
	@case "$(VERSION)" in v[0-9]*) ;; *) echo "release-check wants the tag: make release-check VERSION=v0.2.0 (got \"$(VERSION)\")"; exit 1;; esac; \
	top=$$(awk '/^## /{ sub(/^## /, ""); print; exit }' CHANGELOG.md); \
	want=$$(printf '%s' "$(VERSION)" | sed 's/^v//'); \
	if [ -z "$$top" ]; then echo "CHANGELOG.md has no ## release heading"; exit 1; fi; \
	if [ "$$top" != "$$want" ]; then \
		echo "CHANGELOG.md's newest release is $$top and the tag is $(VERSION); add the release to the changelog and commit it before tagging"; \
		exit 1; \
	fi

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
