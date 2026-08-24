.PHONY: all build build-matrix build-linux-amd64 build-linux-arm64 build-linux-armv6 build-darwin-amd64 build-darwin-arm64 fmt vet lint-frontend run-tests open_coverage clean e2e release-local _all

# Coverage gate. Pinned as a `tool` directive in go.mod (`go get -tool`), so the
# version is tracked there rather than inline here.
COVGATE := go tool covgate


# Version metadata baked into the binary at link time. Override on the
# command line for reproducible release builds: `make build VERSION=v0.1.1`.
VERSION    ?= $(shell cat VERSION 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
	-X github.com/kfet/harb.Version=$(VERSION) \
	-X github.com/kfet/harb.Commit=$(COMMIT) \
	-X github.com/kfet/harb.BuildDate=$(BUILD_DATE)

# Quiet runner: $(call RUN,label,cmd) — runs cmd silently, prints "✓ label" on
# success, dumps captured output and exits non-zero on failure. Set V=1 for
# verbose output.
ifdef V
  define RUN
	@echo "→ $(1)"
	@$(2)
  endef
else
  define RUN
	@_log=$$(mktemp); \
	if ( $(2) ) > $$_log 2>&1; then \
		echo "✓ $(1)"; rm -f $$_log; \
	else \
		rc=$$?; cat $$_log; rm -f $$_log; exit $$rc; \
	fi
  endef
endif

# Default target. Runs gofmt, go vet, race tests + 100%
# coverage gate, and the e2e smoke — all in parallel via a recursive
# `make -j`. Wall-time is roughly the slowest single task, not the sum.
#
# This is exactly what CI runs — no separate "fast" mode. If you want
# to iterate faster locally, run `go test ./...` directly.
all:
	@$(MAKE) -j --no-print-directory _all
	@echo "✓ all green"

# Internal aggregate target — every prereq is independent and self-
# contained, so `make -j` can fan them out.
_all: build build-matrix fmt vet lint-frontend run-tests e2e

# Build the harb binary into ./harb. Standalone target so a plain
# `go build` failure is caught by `make all` without needing the e2e
# harness to run.
build:
	$(call RUN,build ./harb,go build -trimpath -ldflags='$(LDFLAGS)' -o harb ./cmd/harb)

# Cross-compile check across a matrix of targets. Compile-only, no
# artefacts. CGO disabled to ensure portability. Pure-Go so each target
# is just an env-var flip.
build-matrix: build-linux-amd64 build-linux-arm64 build-linux-armv6 build-darwin-amd64 build-darwin-arm64

build-linux-amd64:
	$(call RUN,build linux/amd64,CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -trimpath -ldflags='$(LDFLAGS)' -o /dev/null ./cmd/harb)
build-linux-arm64:
	$(call RUN,build linux/arm64,CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -trimpath -ldflags='$(LDFLAGS)' -o /dev/null ./cmd/harb)
build-linux-armv6:
	$(call RUN,build linux/armv6,CGO_ENABLED=0 GOOS=linux  GOARCH=arm GOARM=6 go build -trimpath -ldflags='$(LDFLAGS)' -o /dev/null ./cmd/harb)
build-darwin-amd64:
	$(call RUN,build darwin/amd64,CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags='$(LDFLAGS)' -o /dev/null ./cmd/harb)
build-darwin-arm64:
	$(call RUN,build darwin/arm64,CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags='$(LDFLAGS)' -o /dev/null ./cmd/harb)

# Build real release artefacts locally: one tarball per OS/arch under
# dist/, plus dist/checksums.txt. Same shape as the CI release workflow
# — handy for verifying install.sh / `harb update` end-to-end
# without cutting a real release. Output is gitignored.
release-local:
	@rm -rf dist && mkdir -p dist
	@for t in linux/amd64 linux/arm64 linux/armv6 darwin/amd64 darwin/arm64; do \
		os=$${t%/*}; arch=$${t#*/}; name="harb-$(VERSION)-$$os-$$arch"; \
		echo "→ $$name"; \
		goarch=$$arch; goarm=; \
		if [ "$$arch" = "armv6" ]; then goarch=arm; goarm=6; fi; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$goarch GOARM=$$goarm go build -trimpath \
			-ldflags='$(LDFLAGS)' -o dist/$$name/harb ./cmd/harb || exit $$?; \
		cp LICENSE README.md dist/$$name/ 2>/dev/null || true; \
		tar -C dist -czf dist/$$name.tar.gz $$name; \
		rm -rf dist/$$name; \
	done
	@cd dist && shasum -a 256 *.tar.gz > checksums.txt
	@echo "✓ release artefacts in dist/"
	@ls -l dist/

fmt:
	$(call RUN,gofmt,gofmt -w .)

vet:
	$(call RUN,go vet clean,go vet ./...)

# Static checks for the bundled CSS / HTML templates / JavaScript.
# Pure-Go (no npm); if `node` is on PATH we also run `node --check`
# on each JS file, otherwise the JS pass is skipped with a notice.
# Fast (~500ms after first build); designed to catch the kinds of
# regressions LLMs / humans keep introducing into these files (bad
# CSS selector lists with at-rules, unterminated comments, broken
# Go template syntax, JS that doesn't parse).
lint-frontend:
	$(call RUN,frontend lint,GOCACHE=$(CURDIR)/.cache/lintfrontend go run ./scripts/lintfrontend ./internal/ui)

# Run unit tests with race + shuffle + fresh cache + 100% coverage gate.
# Standalone — `make all` runs this in parallel with fmt/vet/e2e.
run-tests:
	@go clean -testcache
	$(call RUN,tests pass,go test -race -shuffle=on -cover ./... -coverprofile=coverage.tmp.out)
	$(call RUN,coverage clean,$(COVGATE) -profile=coverage.tmp.out -out=coverage.out -ignore=.covignore -min=100)
	@rm -f coverage.tmp.out

open_coverage:
	go tool cover -html=coverage.out

clean:
	rm -f coverage.out coverage.tmp.out harb
	rm -rf dist

# End-to-end smoke: builds the binary, exercises ClientLogin → subscription
# list → stream/contents → edit-tag → unread-count, plus a UI login + home
# fetch. Run as part of `make all`; also invocable standalone.
e2e:
	$(call RUN,e2e smoke,E2E=1 go test -count=1 -timeout=60s ./e2e/...)
