MODULE   := github.com/Autarquico/codefabric
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse HEAD 2>/dev/null)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w \
	-X $(MODULE)/internal/buildinfo.version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.date=$(DATE)
GORELEASER ?= go run github.com/goreleaser/goreleaser/v2@v2.18.3
TARGETS  := linux/amd64 linux/arm64 darwin/arm64

export CGO_ENABLED := 0

.PHONY: build test test-race test-integration test-e2e test-e2e-property test-e2e-race e2e-reap bench lint fmt cover cross release-snapshot clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/cf ./cmd/cf

test:
	go test ./...

test-race:
	CGO_ENABLED=1 go test -race -count=1 ./...

test-integration:
	CGO_ENABLED=1 go test -race -tags integration -count=1 ./...

bench:
	CF_LOAD_FLOOR=1 go test -tags integration -run TestLoad -v ./internal/controller/store/

# End-to-end suite. E2E_SEEDS random seeds run after the fixed corpus of
# internal/e2e/testdata/seeds.txt (0: the corpus alone); E2E_SEED runs exactly
# that seed instead.
E2E_SEEDS   ?= 0
E2E_SEED    ?=
E2E_TIMEOUT ?= 25m
E2E_SEEDING := CF_E2E_SEEDS=$(E2E_SEEDS) $(if $(E2E_SEED),CF_E2E_SEED=$(E2E_SEED))
E2E_TEST    := go test -tags e2e -count=1 -v -timeout $(E2E_TIMEOUT)

test-e2e: build
	CF_E2E_BIN=$(CURDIR)/bin/cf $(E2E_SEEDING) $(E2E_TEST) ./internal/e2e/...

test-e2e-property: build
	CF_E2E_BIN=$(CURDIR)/bin/cf $(E2E_SEEDING) $(E2E_TEST) -run '^TestProperty_' ./internal/e2e/

# Every cf of the cluster built with -race by TestMain: CF_E2E_BIN is emptied,
# since a binary from outside would run uninstrumented.
test-e2e-race:
	CF_E2E_BIN= CF_E2E_RACE=1 CGO_ENABLED=1 $(E2E_SEEDING) $(E2E_TEST) -race ./internal/e2e/

# Runs no test: TestMain alone reaps the cfe2e roots whose owner is proven dead
# (killing what their manifests started) and leaves live invocations alone.
e2e-reap: build
	CF_E2E_BIN=$(CURDIR)/bin/cf go test -tags e2e -count=1 -v -run '^$$' ./internal/e2e/

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

cross:
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "build $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o bin/cf-$$os-$$arch ./cmd/cf || exit 1; \
	done

release-snapshot:
	$(GORELEASER) release --snapshot --clean

clean:
	rm -rf bin dist coverage.out
