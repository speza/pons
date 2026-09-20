GO_FILES := $(shell find . -type f -name '*.go' -not -path './.git/*' -print)
# Resolve from the Go bin dir so make lint works even when ~/go/bin is
# not on PATH (CI pins the action's binary; install locally with
# `make install-tools`).
GOLANGCI_LINT ?= $(shell go env GOPATH)/bin/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.6.0
LINT_GOTOOLCHAIN ?= go1.25.0

.PHONY: fmt fmt-check test test-race vet lint check install-tools smoke-runtime

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

vet:
	go vet ./...

install-tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint:
	GOTOOLCHAIN=$(LINT_GOTOOLCHAIN) $(GOLANGCI_LINT) run --timeout=5m ./...

check: fmt-check test vet lint

smoke-runtime:
	./scripts/smoke-runtime.sh
