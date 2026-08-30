# hostshift — build, test, package.
#
# There is no `go` on PATH on every machine that builds this; set GO if yours
# differs, e.g. GO=/nix/store/…-go-1.26.5/bin/go make test.
GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X main.version=$(VERSION)
IMAGE   ?= ghcr.io/generoi/hostshift

# The platforms the binary actually has to run on: the DDEV web container
# (linux, both architectures) and the developer's own machine.
PLATFORMS = linux/amd64 linux/arm64 darwin/arm64 darwin/amd64

.PHONY: all
all: test build

.PHONY: build
build:
	$(GO) build -ldflags '$(LDFLAGS)' -o hostshift ./cmd/hostshift

.PHONY: test
test: test-addon
	$(GO) test ./...

# The add-on command is shell, and it is where the opinions live. No Docker and
# no DDEV needed — it wants `hostshift` on PATH, git, and a .ddev/ directory.
.PHONY: test-addon
test-addon:
	GO=$(GO) bash test/addon-command.sh

# The same command, but through real DDEV and a real router: what gets served,
# rather than what gets written. Needs Docker and DDEV; skips cleanly without
# them. Defaults to the published image, which is what `ddev add-on get` gives a
# developer and where image skew shows up.
.PHONY: test-integration
test-integration:
	GO=$(GO) bash test/integration-ddev.sh
	GO=$(GO) bash test/integration-proxy-ddev.sh

# The invariant that guards everything else (PLAN §5.2). If this goes red,
# nothing downstream of it is trustworthy.
.PHONY: identity
identity:
	$(GO) test ./internal/rewrite/ -run 'TestIdentityMapByteIdentity|TestIdentityHoldsAtEveryChunkSize' -v

.PHONY: vet
vet:
	$(GO) vet ./...
	gofmt -l ./cmd ./internal

.PHONY: dist
dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  echo "  $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/hostshift-$$os-$$arch ./cmd/hostshift || exit 1; \
	done
	@ls -lh dist/

.PHONY: image
image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

.PHONY: clean
clean:
	rm -rf hostshift dist/
