GO ?= go
BINARY ?= flow-go

.PHONY: all build run test test-race vet fmt check tidy stats cookies clean help

all: build

## build: compile the binary
build:
	$(GO) build -o $(BINARY) .

## run: listen for the extension and persist account bundles (no HTTP API)
run:
	$(GO) run . bridge

## test: run the test suite
test:
	$(GO) test ./...

## test-race: run the test suite under the race detector
test-race:
	$(GO) test -race ./...

## vet: run static analysis
vet:
	$(GO) vet ./...

## fmt: format the source
fmt:
	$(GO) fmt ./...

## check: formatting, vet, then the suite — the same set CI would run
check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; fi
	$(GO) vet ./...
	$(GO) test ./...

## tidy: reconcile go.mod and go.sum
tidy:
	$(GO) mod tidy

## stats: print database statistics
stats:
	$(GO) run . stats

## cookies: show cookie and credential status
cookies:
	$(GO) run . cookies

## clean: remove the binary and build artifacts
clean:
	rm -f $(BINARY)
	$(GO) clean -testcache

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
