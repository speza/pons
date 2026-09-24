GO_FILES := $(shell find . -type f -name '*.go' -not -path './.git/*' -print)
# Resolve from the Go bin dir so make lint works even when ~/go/bin is
# not on PATH (CI pins the action's binary; install locally with
# `make install-tools`).
GOLANGCI_LINT ?= $(shell go env GOPATH)/bin/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.13.2
LINT_GOTOOLCHAIN ?= go1.27.1
E2B_CLI_VERSION ?= 2.20.0

.PHONY: fmt fmt-check test test-race test-integration test-integration-race test-integration-seatbelt test-integration-e2b vet lint check install-tools smoke-runtime smoke-e2b cleanup-e2b cleanup-e2b-all build-e2b-hands e2b-template web-install web-check web-build

fmt:
	@if [ -n "$(GO_FILES)" ]; then gofmt -w $(GO_FILES); fi

fmt-check:
	@if [ -z "$(GO_FILES)" ]; then exit 0; fi; \
	files="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$files" ]; then \
		echo "gofmt required:"; \
		echo "$$files"; \
		exit 1; \
	fi

test:
	go test ./...

test-race:
	go test -race ./...

test-integration:
	go test -tags integration -count=1 ./integration

test-integration-race:
	go test -race -tags integration -count=1 ./integration

test-integration-seatbelt:
	@if [ "$$(uname -s)" != "Darwin" ]; then echo "Seatbelt integration test requires macOS"; exit 0; fi
	PONS_SEATBELT_TEST=1 go test -tags integration -count=1 ./integration ./environment/seatbelt

test-integration-e2b:
	@test -n "$$E2B_API_KEY" || (echo "E2B_API_KEY is required"; exit 1)
	PONS_E2B_TEST=1 go test -tags integration -count=1 -run '^TestE2BWorkspaceCheckpointRecovery$$' ./environment/e2b

vet:
	go vet ./...

install-tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint:
	GOTOOLCHAIN=$(LINT_GOTOOLCHAIN) $(GOLANGCI_LINT) run --timeout=5m ./...

check: fmt-check test vet lint

smoke-runtime:
	./scripts/smoke-runtime.sh

smoke-e2b:
	./scripts/smoke-e2b.sh

cleanup-e2b:
	./scripts/cleanup-e2b.sh

cleanup-e2b-all:
	./scripts/cleanup-e2b.sh --all

build-e2b-hands:
	@mkdir -p .build
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o .build/pons-hands-linux-amd64 ./cmd/pons-hands

e2b-template: build-e2b-hands
	@test -n "$$E2B_API_KEY" || (echo "E2B_API_KEY is required"; exit 1)
	npx --yes @e2b/cli@$(E2B_CLI_VERSION) template create pons-hands --dockerfile e2b.Dockerfile

web-install:
	npm --prefix web install

web-check:
	npm --prefix web run check

web-build:
	npm --prefix web run build
