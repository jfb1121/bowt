BINARY  := bowt
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: help build test race vet fmt fmt-check lint check install tidy clean

help: ## show this help
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*## /\t/' | column -t -s $$'\t'

build: ## compile the binary (version-stamped)
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

test: ## run tests
	go test ./...

race: ## run tests under the race detector
	go test -race ./...

vet: ## go vet
	go vet ./...

fmt: ## format all code
	gofmt -w .

fmt-check: ## fail if any file is unformatted
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "unformatted:"; echo "$$unformatted"; exit 1; fi

lint: ## run golangci-lint
	golangci-lint run

check: fmt-check vet lint race ## everything CI enforces

install: ## go install (version-stamped)
	go install -ldflags "$(LDFLAGS)" .

tidy: ## tidy modules
	go mod tidy

clean: ## remove build artifacts
	rm -f $(BINARY)
