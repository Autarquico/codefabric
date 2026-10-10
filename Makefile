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

.PHONY: build test test-race test-integration test-e2e lint fmt cover cross release-snapshot clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/cf ./cmd/cf

test:
	go test ./...

test-race:
	CGO_ENABLED=1 go test -race ./...

test-integration:
	go test -tags integration ./...

test-e2e: build
	go test -tags e2e ./internal/e2e/...

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
