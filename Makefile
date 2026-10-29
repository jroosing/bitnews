VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
FUZZTIME ?= 10s
PLATFORMS ?= linux/amd64
REGISTRY_IMAGE ?= ghcr.io/jroosing/bitnews

# Pinned 7-Zip and par2cmdline-turbo (ADR-0007), downloaded into bin/tools/.
# The tests that exercise them find them through PATH.
TOOLS_BIN := $(CURDIR)/bin/tools

# image-push builds the linux/amd64 image and pushes it to ghcr.io
# from this machine, tagged $(VERSION) and latest. Log in first (docs/deploy.md).
.PHONY: zig zig-check blackbox-zig image-push tools build build-all fmt fmt-check fix fix-check test e2e lint vuln fuzz-smoke check css css-watch css-check generate generate-check

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/bitnews ./cmd/bitnews

image-push:
	docker buildx build --platform linux/amd64 \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY_IMAGE):latest -t $(REGISTRY_IMAGE):$(VERSION) --push .

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

check: fmt-check fix-check generate-check css-check lint test e2e vuln

# The web UI stylesheet. internal/web/assets/static/app.css is generated and
# committed, so `go build` and CI need no Tailwind step. Run `make css` after
# changing a .templ file, internal/web/src/app.css or internal/web/assets/static/js.
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

# The web UI views (ADR-0011). internal/web/*_templ.go is generated from the
# .templ files and committed, so `go build` and CI need no generate step. The
# generator is pinned through the tool directive in go.mod. Run `make generate`
# after changing a .templ file.
generate:
	go tool templ generate -path internal/web

# Fails when a committed *_templ.go is out of date (it leaves the regenerated
# files in place). Part of `check`, not of CI.
generate-check:
	@sum() { find internal/web -name '*_templ.go' | sort | xargs cat 2>/dev/null | cksum; }; \
	before=$$(sum); go tool templ generate -path internal/web >/dev/null || exit 1; after=$$(sum); \
	if [ "$$before" = "$$after" ]; then echo "templ up to date"; \
	else echo "*_templ.go is out of date (run 'make generate')"; exit 1; fi

# ---- the Zig rewrite (branch zig, M15, ADR-0014) --------------------------------------

# What the Zig daemon does not do yet, by milestone: the data routes and events
# (Z2), the admin account and the web pages (Z6). Shrinks as they land.
ZIG_BLACKBOX_SKIP := TestARequestBodyAboveTheRouteLimit|TestAdminPasswordFile|TestData|TestShutdownEndsAnOpenEventStream|TestWeb|TestWhileSetupIsOpenTheSetupToken

zig:
	@cd zig && $$(../scripts/get-zig.sh) build

# The Go black-box suite (internal/blackbox) against the Zig binary.
blackbox-zig: zig
	BITNEWS_E2E_BINARY=$(CURDIR)/zig/zig-out/bin/bitnews go test -count=1 -skip '$(ZIG_BLACKBOX_SKIP)' ./internal/blackbox/

# The Zig gate: format, tests and shared files, then the parity suite.
zig-check:
	@cd zig && $$(../scripts/get-zig.sh) build check
	@$(MAKE) --no-print-directory blackbox-zig
