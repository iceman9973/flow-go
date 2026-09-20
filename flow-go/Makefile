GO ?= go
BINARY ?= flow-go

.PHONY: all build run test vet fmt tidy clean stats help

all: build

## build: compile the binary
build:
	$(GO) build -o $(BINARY) .

## run: start the API and the extension bridge
run:
	$(GO) run . serve

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
