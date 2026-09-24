GO ?= go
BINARY ?= flow

LOG_FILE ?= flow.log

# clean-data and reset delete the account bundles, which cannot be rebuilt without
# re-syncing from the browser. They refuse to run without CONFIRM=yes rather than
# taking the session out from under someone who typed the target by accident.
CONFIRM ?=

.PHONY: all build run test test-race test-cover vet fmt check tidy \
        doctor projects stats cookies smoke-image \
        clean clean-data reset help

all: build

## build: compile the binary
build:
	$(GO) build -o $(BINARY) .

## run: listen for the extension in the foreground (no HTTP API)
run:
	$(GO) run . bridge

## test: run the test suite
test:
	$(GO) test ./...

## test-race: run the test suite under the race detector
test-race:
	$(GO) test -race ./...

## test-cover: run the test suite with per-package coverage
test-cover:
	$(GO) test -cover ./...

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

## doctor: check the extension, the session and the project — exits 1 if any is unhealthy
doctor:
	$(GO) run . doctor

## projects: list the account's Flow projects, newest first
projects:
	$(GO) run . projects

## stats: print database statistics
stats:
	$(GO) run . stats

## cookies: show cookie and credential status
cookies:
	$(GO) run . cookies

## smoke-image: one image generation end to end — SPENDS ONE CREDIT
smoke-image:
	@echo "smoke-image submits a real generation and spends one credit."
	$(GO) run . image "a serene minimalist Japanese zen garden, perfectly balanced smooth dark river stones and a delicate ancient bonsai tree on mossy ground, concentric raked white sand ripples, soft warm golden hour sunlight, ultra-detailed macro photography, 8k"

## clean: remove the binary, build artifacts, and logs
clean:
	rm -f $(BINARY) $(LOG_FILE)
	$(GO) clean -testcache

## clean-data: delete cookies/, data/ and output/ (requires CONFIRM=yes)
clean-data:
	@if [ "$(CONFIRM)" != "yes" ]; then \
		echo "clean-data removes:"; \
		echo "  cookies/   every account bundle — re-syncable only from the browser"; \
		echo "  data/      the SQLite database, including generation history"; \
		echo "  output/    every downloaded image and video"; \
		echo; \
		echo "The account bundles are the expensive part: without them every run"; \
		echo "needs the extension attached and signed in again."; \
		echo; \
		echo "Re-run as:  make clean-data CONFIRM=yes"; \
		exit 1; \
	fi
	rm -rf cookies data output
	@echo "removed cookies/, data/ and output/"

## reset: clean, then clean-data — a full clean slate (requires CONFIRM=yes)
reset:
	@if [ "$(CONFIRM)" != "yes" ]; then \
		echo "reset runs 'clean' and 'clean-data': a full clean slate, including"; \
		echo "every account bundle, the SQLite database and every downloaded asset."; \
		echo; \
		echo "The account bundles are the expensive part: without them every run"; \
		echo "needs the extension attached and signed in again."; \
		echo; \
		echo "Re-run as:  make reset CONFIRM=yes"; \
		exit 1; \
	fi
	@$(MAKE) --no-print-directory clean
	@$(MAKE) --no-print-directory clean-data CONFIRM=yes

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
