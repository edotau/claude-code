# =============================================================================
# claude-code — recipe wrappers for build/install/test.
#
# Run `make help` for the catalogue.
# Scope: a robust build+install story for THIS repo — no Databricks/OpenShift/VS Code/workspace-sync targets.
# =============================================================================
SHELL          := /bin/bash
.SHELLFLAGS    := -euo pipefail -c
MAKEFLAGS      += --no-print-directory

# Status-line colors for the echo -e prefixes below; NO_COLOR=1 falls back to plain text.
C_EMPTY :=
C_SP    := $(C_EMPTY) $(C_EMPTY)
ifdef NO_COLOR
C_OK   := OK$(C_SP)
C_NOTE := note:$(C_SP)
else
C_OK   := \033[1;32mOK\033[0m$(C_SP)
C_NOTE := \033[33mnote:\033[0m$(C_SP)
endif

# Go toolchain: a `go` on PATH wins; otherwise the user-local install at ~/.local/go (no sudo).
# GO_VERSION follows go.mod's `go` line so bootstrap and CI (setup-go go-version-file) agree.
GO_LOCAL_ROOT   ?= $(HOME)/.local/go
GO              ?= $(shell command -v go 2>/dev/null || echo $(GO_LOCAL_ROOT)/bin/go)
GO_VERSION      := $(shell awk '/^go /{print $$2; exit}' go.mod)
# Recipes see the local toolchain and GOPATH/bin (govulncheck) even when the caller's PATH lacks them.
export PATH     := $(GO_LOCAL_ROOT)/bin:$(HOME)/go/bin:$(PATH)
BINARY          := claude-code
BIN             := bin/$(BINARY)
PKGS            := ./...
# Mirrors .github/workflows/ci.yml's `gofmt -l cmd internal` exactly — audit must stay a superset.
GO_FMT_DIRS     := cmd internal
GO_TEST_TIMEOUT ?= 300s
# Repo-owned shell scripts (skill scripts included); lint-shell no-ops when the wildcard is empty.
GO_SH_SRC       := $(wildcard scripts/*.sh scripts/lib/*.sh skills/*/scripts/*.sh)
# Gate tools are PINNED: `@latest` could change under us between runs. `make install DEV_TOOLS=1` installs all three.
GOVULN_VERSION  ?= v1.8.0
GOLANGCI_VERSION ?= v2.15.0
YAMLFMT_VERSION ?= v0.21.0
# Markdown linter/fixer, installed to bin/ by `make install`; its pkg/markdown is the Go code's parser too.
MDSMITH_VERSION ?= v0.57.0
MDSMITH         := bin/mdsmith
YAML_FILES      := $(shell git ls-files '*.yml' '*.yaml' 2>/dev/null)

.DEFAULT_GOAL := help

##@ Go
.PHONY: compile
compile:  ## Compile bin/claude-code, no install
	$(GO) build -trimpath -ldflags='-s -w' -o $(BIN) ./cmd/claude-code

.PHONY: build
build: install  # (dev recipe, hidden from help) compile + install + audit (SKIP_AUDIT=1)
	@$(if $(SKIP_AUDIT),echo -e "$(C_NOTE)audit skipped (SKIP_AUDIT)",$(MAKE) audit)

# install.go already links the bare shims, renders settings.json, and writes bash completion
# (internal/launch/install.go: Install() -> completion.Install) — one call does all three.
.PHONY: install
install: compile mdsmith  ## Build + install (DEV_TOOLS=1: lint/vuln tools)
	$(BIN) install
	@if [ "$(DEV_TOOLS)" = 1 ]; then \
		$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULN_VERSION) && \
		$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) && \
		$(GO) install github.com/google/yamlfmt/cmd/yamlfmt@$(YAMLFMT_VERSION); fi
	@if $(BIN) router status 2>&1 | grep -qE '^router[[:space:]]+running'; then \
		$(BIN) router stop >/dev/null && $(BIN) router start >/dev/null; \
		echo -e "$(C_OK)router restarted (was serving the previous binary)"; \
	fi

.PHONY: clean
clean:  ## Remove bin/claude-code
	@rm -f $(BIN)
	@echo -e "$(C_OK)cleaned: $(BIN)"

.PHONY: test
test: audit  ## Full gate (= audit)

.PHONY: vet
vet:  # (dev recipe, hidden from help) go vet
	$(GO) vet $(PKGS)

##@ Setup
.PHONY: help-svg
help-svg: compile  ## Redraw the README menu image
	$(BIN) help-svg --out docs/images/cli-menu.svg

.PHONY: bootstrap
bootstrap:  ## Fresh box: Go, install, configure (CHECK=1 TAILSCALE=1)
	@$(MAKE) go-toolchain
	@$(MAKE) install
	@$(if $(filter 1,$(TAILSCALE)),$(MAKE) tailscale-cli)
	@$(if $(filter 1,$(CHECK)),$(BIN) configure --check,$(BIN) configure)

INTO ?= $(HOME)/.claude
.PHONY: adopt
adopt:  ## Stage clone: adopt INTO (~/.claude), then bootstrap
	@if [ -e "$(INTO)/.git" ]; then echo "$(INTO) is already a checkout"; exit 0; fi; \
	$(MAKE) go-toolchain compile && \
	$(BIN) install --setup adopt --from "$(CURDIR)" --into "$(INTO)" $(ADOPT_ARGS) && \
	$(if $(findstring --dry-run,$(ADOPT_ARGS)),true,$(MAKE) -C "$(INTO)" bootstrap)

.PHONY: tailscale-cli
tailscale-cli:  ## Install the Tailscale client
	@if command -v tailscale >/dev/null 2>&1; then echo -e "$(C_OK)tailscale found: $$(tailscale version | head -1)"; exit 0; fi; \
	case "$$(uname -s)" in \
	  Darwin) brew install --cask tailscale-app ;; \
	  Linux)  sh -c "$$(curl -fsSL https://tailscale.com/install.sh)" ;; \
	  *) echo "install tailscale manually: https://tailscale.com/download"; exit 1 ;; \
	esac

.PHONY: go-toolchain
go-toolchain:  ## Install go.mod's Go to ~/.local/go if missing
	@if command -v go >/dev/null 2>&1; then echo -e "$(C_OK)go found: $$(go version) ($$(command -v go))"; exit 0; fi; \
	os=$$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$$(uname -m); \
	case "$$arch" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; esac; \
	tgz="go$(GO_VERSION).$$os-$$arch.tar.gz"; url="https://dl.google.com/go/$$tgz"; tmp=$$(mktemp -d); \
	echo "downloading $$url"; \
	curl -fsSL -o "$$tmp/$$tgz" "$$url" && curl -fsSL -o "$$tmp/$$tgz.sha256" "$$url.sha256"; \
	got=$$( (sha256sum "$$tmp/$$tgz" 2>/dev/null || shasum -a 256 "$$tmp/$$tgz") | cut -d' ' -f1 ); \
	[ "$$got" = "$$(cat "$$tmp/$$tgz.sha256")" ] || { echo "sha256 mismatch for $$tgz"; rm -rf "$$tmp"; exit 1; }; \
	tar -C "$$tmp" -xzf "$$tmp/$$tgz" && mkdir -p "$(dir $(GO_LOCAL_ROOT))" && mv "$$tmp/go" "$(GO_LOCAL_ROOT)"; \
	rm -rf "$$tmp"; \
	echo -e "$(C_OK)installed $$($(GO_LOCAL_ROOT)/bin/go version) -> $(GO_LOCAL_ROOT)"; \
	echo -e "$(C_NOTE)add it to your shell: make setup-env APPLY=1"

.PHONY: setup-env
setup-env:  ## Print the PATH line (APPLY=1 writes shell rc)
	@line='export PATH="$$HOME/.claude/bin:$$HOME/.local/bin:$$HOME/.local/go/bin:$$HOME/go/bin:$$PATH"'; \
	echo "$$line"; \
	if [ -n "$(APPLY)" ]; then \
		rc="$$HOME/.bashrc"; case "$${SHELL:-}" in */zsh) rc="$$HOME/.zshrc";; esac; \
		if grep -qxF "$$line" "$$rc" 2>/dev/null; then \
			echo -e "$(C_NOTE)already present in $$rc"; \
		else \
			printf '\n# claude-code: ~/.claude/bin, ~/.local/bin, ~/.local/go/bin, ~/go/bin on PATH\n%s\n' "$$line" >> "$$rc"; \
			echo -e "$(C_OK)appended to $$rc"; \
		fi; \
	fi

##@ Quality
# Umbrella gate: gofmt + vet + test + build (ci.yml's four steps, same order) + govulncheck when
# installed + the agents sync --dry-run smoke test. Exact superset of ci.yml (which runs this target).
.PHONY: audit
audit: compile  ## CI gate: fmt, vet, test, build, vuln, docs
	@out="$$(gofmt -l $(GO_FMT_DIRS))"; if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi
	$(GO) vet $(PKGS)
	$(GO) test -timeout $(GO_TEST_TIMEOUT) $(PKGS)
	$(GO) build $(PKGS)
	@echo -e "$(C_OK)go check passed (gofmt + vet + test + build)"
	@if command -v govulncheck >/dev/null 2>&1; then govulncheck $(PKGS); \
		else echo -e "$(C_NOTE)govulncheck not installed — skipped (make install DEV_TOOLS=1)"; fi
	@$(MAKE) --no-print-directory lint-yaml
	@CLAUDE_CONFIG_DIR=$(CURDIR) $(BIN) agents sync --dry-run >/dev/null
	@t=$$(mktemp) && $(BIN) help-svg --out $$t && cmp -s $$t docs/images/cli-menu.svg; rc=$$?; rm -f $$t; \
		if [ $$rc -ne 0 ]; then echo "docs/images/cli-menu.svg is stale: run make help-svg"; exit 1; fi
	@echo -e "$(C_OK)audit passed (gofmt/vet/test/build + sync smoke + generated docs; vuln + yaml when installed)"

.PHONY: fmt
fmt:  ## gofmt -w every Go file
	$(GO) fmt $(PKGS) >/dev/null
	@echo -e "$(C_OK)fmt applied (gofmt)"

.PHONY: lint
lint: lint-go lint-shell lint-yaml lint-help lint-settings lint-docs  ## vet, shell, yml, tui width, settings, md fix (CHECK=1)
	@echo -e "$(C_OK)lint passed"

.PHONY: lint-go
lint-go:  # (folded into `lint`) gofmt -l + go vet
	@out="$$(gofmt -l $(GO_FMT_DIRS))"; if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi
	$(GO) vet $(PKGS)
	@echo -e "$(C_OK)gofmt + vet clean"

.PHONY: lint-deep
lint-deep:  # (dev recipe, hidden from help) golangci-lint run with .golangci.yml
	golangci-lint run --max-issues-per-linter=0 --max-same-issues=0 ./...

.PHONY: lint-help
lint-help:  # (folded into `lint`) every `make help` row fits 80 columns, label included
	@out="$$($(MAKE) --no-print-directory help | sed 's/\x1b\[[0-9;]*m//g' | awk 'length>80')"; \
		if [ -n "$$out" ]; then echo "help rows over 80 columns:"; echo "$$out"; exit 1; fi

.PHONY: lint-yaml
lint-yaml:  # (folded into `lint` and `audit`) yamlfmt -lint over tracked YAML (.yamlfmt keeps blank lines)
	@if ! command -v yamlfmt >/dev/null 2>&1; then echo -e "$(C_NOTE)yamlfmt not installed — skipped (make install DEV_TOOLS=1)"; \
		elif [ -n "$(YAML_FILES)" ]; then yamlfmt -lint $(YAML_FILES); fi

.PHONY: mdsmith
mdsmith:  # (folded into `install`) pinned mdsmith binary in bin/
	@if ! $(MDSMITH) version 2>/dev/null | grep -qF '$(MDSMITH_VERSION)'; then \
		GOBIN=$(CURDIR)/bin $(GO) install github.com/jeduden/mdsmith/cmd/mdsmith@$(MDSMITH_VERSION); fi

.PHONY: lint-docs
lint-docs: mdsmith  # (folded into `lint`) mdsmith fix over .mdsmith.yml files; CHECK=1 only reports
	@$(MDSMITH) $(if $(filter 1,$(CHECK)),check,fix)

.PHONY: lint-shell
lint-shell:  # (folded into `lint`) shellcheck -S error over repo-owned shell scripts, if any exist
	@if [ -z "$(GO_SH_SRC)" ]; then echo -e "$(C_NOTE)no shell scripts to lint"; \
	elif command -v shellcheck >/dev/null 2>&1; then shellcheck -S error $(GO_SH_SRC) && echo -e "$(C_OK)shellcheck clean"; \
	else echo -e "$(C_NOTE)shellcheck not on PATH — skipped"; fi

.PHONY: lint-settings
lint-settings: compile  # (folded into `lint`) vendor-config render drift, against THIS checkout
	CLAUDE_CONFIG_DIR=$(CURDIR) $(BIN) generate --check

.PHONY: verify
verify: audit  ## audit + doctor + router status
	@$(BIN) doctor
	@$(BIN) router status
	@echo -e "$(C_OK)verify passed (audit + doctor + router status)"

##@ Diagnostics
.PHONY: doctor
doctor: compile  ## Harness health checks
	$(BIN) doctor

.PHONY: router
router: compile  ## Router daemon status (UP=1 starts it)
	@if [ -n "$(UP)" ]; then $(BIN) router start; fi
	$(BIN) router status

.PHONY: statusline
statusline: compile  ## Render a sample statusline (COLS=…)
	@echo '{"model":{"id":"claude-sonnet-5","display_name":"Sonnet 5"},"workspace":{"current_dir":"'$(CURDIR)'"},"transcript_path":"/tmp/x","context_window":{"context_window_size":1000000,"used_percentage":42}}' \
		| COLUMNS=$${COLS:-120} $(BIN) statusline

##@ Quick Reference
.PHONY: help
help:  ## Show this help
	@printf "\n\033[1mclaude-code\033[0m\n"
	@printf "\033[2mUsage:\033[0m make <target> [VAR=value]\n"
	@awk 'BEGIN{FS=":"} \
		/^##@/{sec=substr($$0,5); if(!(sec in o)) o[sec]=++n; next} \
		/^[a-zA-Z0-9_.%\/-]+:[^=]/{t=$$1; if(t ~ /^[._]/) next; \
			if(!match($$0,/##[ \t]*/)) next; d=substr($$0,RSTART+RLENGTH); \
			if(t in seen) next; seen[t]=1; \
			printf "%d\t%s\t%s\t%s\n", o[sec], sec, t, d}' $(MAKEFILE_LIST) \
		| sort -t "$$(printf '\t')" -k1,1n -k3,3 \
		| awk -F"\t" '{if($$2!=cur){cur=$$2; printf "\n\033[1m%s\033[0m\n", $$2} \
			printf "  \033[36m%-16s\033[0m %s\n", $$3, $$4}'
	@printf "\n"
