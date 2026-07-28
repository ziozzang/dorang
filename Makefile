GO      ?= go
PKG     := ./...
BIN     := bin
LDFLAGS := -s -w

.PHONY: all build test test-race test-integration lint vet fmt cover bench clean deps-up deps-down

all: build

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/dorang    ./cmd/dorang
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/dorangctl ./cmd/dorangctl

test:
	$(GO) test $(PKG)

test-race:
	$(GO) test -race -count=1 $(PKG)

# Requires `make deps-up`.
test-integration:
	DORANG_TEST_PG=postgres://dorang:dorang@127.0.0.1:55432/dorang?sslmode=disable \
	DORANG_TEST_REDIS=redis://127.0.0.1:56379 \
	$(GO) test -tags=integration -count=1 $(PKG)

bench:
	$(GO) test -run '^$$' -bench . -benchmem $(PKG)

cover:
	$(GO) test -coverprofile=coverage.out $(PKG)
	$(GO) tool cover -func=coverage.out | tail -1

vet:
	$(GO) vet $(PKG)

fmt:
	$(GO) fmt $(PKG)

lint: vet
	@command -v staticcheck >/dev/null && staticcheck $(PKG) || echo "staticcheck not installed, skipping"
	@command -v govulncheck  >/dev/null && govulncheck  $(PKG) || echo "govulncheck not installed, skipping"

deps-up:
	docker compose -f deploy/docker-compose.test.yml up -d --wait

deps-down:
	docker compose -f deploy/docker-compose.test.yml down -v

clean:
	rm -rf $(BIN) coverage.out
