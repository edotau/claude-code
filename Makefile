# claude-code harness — build, install, test.
GO    ?= go
BIN   := bin/claude-code
PKGS  := ./...
GOSRC := $(shell find cmd internal -name '*.go' 2>/dev/null)

.PHONY: help build install test lint fmt

help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-8s %s\n", $$1, $$2}'

build: ## compile bin/claude-code
	$(GO) build -o $(BIN) ./cmd/claude-code

install: build ## build, link shims into ~/.claude/bin, render settings.json
	$(BIN) install

test: ## go vet + the full test suite (run once, before pushing)
	$(GO) vet $(PKGS)
	$(GO) test $(PKGS)

lint: ## fail if any Go file is not gofmt-clean
	@out="$$(gofmt -l $(GOSRC))"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

fmt: ## gofmt -w every Go file
	gofmt -w $(GOSRC)
