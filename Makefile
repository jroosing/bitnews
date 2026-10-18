VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
FUZZTIME ?= 10s
PLATFORMS ?= linux/amd64 linux/arm64

.PHONY: build build-all fmt fmt-check fix fix-check test lint vuln fuzz-smoke check

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

test:
	go test -race ./...

lint:
	go vet ./...
	go tool staticcheck ./...

vuln:
	go tool govulncheck ./...

# Runs every Fuzz* target in every package for $(FUZZTIME).
fuzz-smoke:
	@for pkg in $$(go list ./...); do \
		for fn in $$(go test -list '^Fuzz' $$pkg | grep '^Fuzz' || true); do \
			echo "fuzz $$pkg $$fn"; \
			go test -run '^$$' -fuzz "^$$fn$$" -fuzztime $(FUZZTIME) $$pkg || exit 1; \
		done; \
	done

check: fmt-check fix-check lint test vuln
