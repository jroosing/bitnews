VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
FUZZTIME ?= 10s
PLATFORMS ?= linux/amd64

# Pinned 7-Zip and par2cmdline-turbo (ADR-0007), downloaded into bin/tools/.
# The tests that exercise them find them through PATH.
TOOLS_BIN := $(CURDIR)/bin/tools

.PHONY: tools build build-all fmt fmt-check fix fix-check test e2e lint vuln fuzz-smoke check css css-watch css-check

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/bitnews ./cmd/bitnews

build-all:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "build $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/bitnews-$$os-$$arch ./cmd/bitnews || exit 1; \
	done

# fmt and fix rewrite files; fmt-check and fix-check only report (used by CI).
fmt:
	gofmt -s -w .

fmt-check:
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then echo "gofmt needed (run 'make fmt'):"; echo "$$out"; exit 1; fi

fix:
	go fix ./...

fix-check:
	@out=$$(go fix -diff ./...) || exit 1; \
	if [ -n "$$out" ]; then echo "go fix needed (run 'make fix'):"; echo "$$out"; exit 1; fi

tools:
	@scripts/get-postproc-tools.sh >/dev/null

# test is what CI runs. The end-to-end tests (build tag e2e) are not part of it.
test: tools
	PATH="$(TOOLS_BIN):$$PATH" go test -race ./...

# e2e runs the end-to-end tests of the daemon (build tag e2e): a whole daemon
# with fake NNTP servers and a local torrent swarm. Local only, not in CI.
e2e: tools
	PATH="$(TOOLS_BIN):$$PATH" go test -race -tags e2e ./internal/daemon/...

# lint also compiles and checks the e2e tests (-tags e2e), which CI does not run.
lint:
	go vet -tags e2e ./...
	go tool staticcheck -tags e2e ./...

vuln:
	go tool govulncheck ./...

# Runs every Fuzz* target in every package for $(FUZZTIME).
fuzz-smoke: tools
	@for pkg in $$(go list ./...); do \
		for fn in $$(go test -list '^Fuzz' $$pkg | grep '^Fuzz' || true); do \
			echo "fuzz $$pkg $$fn"; \
			PATH="$(TOOLS_BIN):$$PATH" go test -run '^$$' -fuzz "^$$fn$$" -fuzztime $(FUZZTIME) $$pkg || exit 1; \
		done; \
	done

check: fmt-check fix-check lint test e2e vuln

# The web UI stylesheet. internal/web/assets/static/app.css is generated and
# committed, so `go build` and CI need no Tailwind step. Run `make css` after
# changing a template, internal/web/src/app.css or internal/web/assets/static/js.
# The standalone CLI (no node) is downloaded and checksum-verified into
# bin/tools/ by scripts/get-tailwind.sh.
WEB_CSS_SRC := internal/web/src/app.css
WEB_CSS_OUT := internal/web/assets/static/app.css

css:
	@tw=$$(scripts/get-tailwind.sh) && $$tw -i $(WEB_CSS_SRC) -o $(WEB_CSS_OUT) --minify

css-watch:
	@tw=$$(scripts/get-tailwind.sh) && $$tw -i $(WEB_CSS_SRC) -o $(WEB_CSS_OUT) --watch

# Fails when the committed stylesheet is out of date. Not part of `check`
# or CI (they need no Tailwind binary).
css-check:
	@tw=$$(scripts/get-tailwind.sh) && tmp=$$(mktemp) && \
		$$tw -i $(WEB_CSS_SRC) -o $$tmp --minify >/dev/null 2>&1 && \
		if cmp -s $$tmp $(WEB_CSS_OUT); then rm -f $$tmp; echo "css up to date"; \
		else rm -f $$tmp; echo "$(WEB_CSS_OUT) is out of date (run 'make css')"; exit 1; fi
