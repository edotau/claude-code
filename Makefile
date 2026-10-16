# =============================================================================
# claude-code — recipe wrappers for build/install/test.
#
# Run `make help` for the catalogue.
# Shape ported from claude-dbx's Makefile (that harness is being retired); scope narrowed to a
# robust build+install story for THIS repo — no Databricks/OpenShift/VS Code/workspace-sync targets.
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

GO              ?= go
BINARY          := claude-code
BIN             := bin/$(BINARY)
PKGS            := ./...
# Mirrors .github/workflows/ci.yml's `gofmt -l cmd internal` exactly — audit must stay a superset.
GO_FMT_DIRS     := cmd internal
GO_TEST_TIMEOUT ?= 300s
# Repo-owned shell scripts only (none today; wildcard is empty and lint-shell no-ops).
GO_SH_SRC       := $(wildcard scripts/*.sh scripts/lib/*.sh)
# Gate tool is PINNED: `@latest` could change under us between runs.
GOVULN_VERSION  ?= v1.8.0

.DEFAULT_GOAL := help

##@ Go
.PHONY: compile
compile:  ## Compile bin/claude-code, no install
	$(GO) build -o $(BIN) ./cmd/claude-code

.PHONY: build
build: install  ## Compile + install + audit (SKIP_AUDIT=1)
	@$(if $(SKIP_AUDIT),echo -e "$(C_NOTE)audit skipped (SKIP_AUDIT)",$(MAKE) audit)

# install.go already links the bare shims, renders settings.json, and writes bash completion
# (internal/launch/install.go: Install() -> completion.Install) — one call does all three.
.PHONY: install
install: compile  ## Binary, shims, settings.json, completion; router restart
	$(BIN) install
	@if $(BIN) router status 2>&1 | grep -qE '^router[[:space:]]+running'; then \
		$(BIN) router stop >/dev/null && $(BIN) router start >/dev/null; \
		echo -e "$(C_OK)router restarted (was serving the previous binary)"; \
	else \
		echo -e "$(C_NOTE)router not running"; \
	fi

.PHONY: clean
clean:  ## Remove bin/claude-code
	@rm -f $(BIN)
	@echo -e "$(C_OK)cleaned: $(BIN)"

.PHONY: test
test: audit  ## Full gate (= audit)

.PHONY: vet
vet:  ## go vet
	$(GO) vet $(PKGS)

##@ Setup
# Umbrella gate: gofmt + vet + test + build (ci.yml's four steps, same order) + govulncheck when
# installed + the same two agents docs/sync --dry-run smoke tests ci.yml runs. Exact superset.
.PHONY: audit
audit: compile  ## CI gate: fmt, vet, test, build, vuln, docs smoke
	@out="$$(gofmt -l $(GO_FMT_DIRS))"; if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi
	$(GO) vet $(PKGS)
	$(GO) test -timeout $(GO_TEST_TIMEOUT) $(PKGS)
	$(GO) build $(PKGS)
	@echo -e "$(C_OK)go check passed (gofmt + vet + test + build)"
	@if command -v govulncheck >/dev/null 2>&1; then govulncheck $(PKGS); \
		else echo -e "$(C_NOTE)govulncheck not installed — skipped (go install golang.org/x/vuln/cmd/govulncheck@$(GOVULN_VERSION))"; fi
	@CLAUDE_CONFIG_DIR=$(CURDIR) $(BIN) agents docs --dry-run >/dev/null
	@CLAUDE_CONFIG_DIR=$(CURDIR) $(BIN) agents sync --gemini --dry-run >/dev/null
	@echo -e "$(C_OK)audit passed (gofmt/vet/test/build + vuln + docs/sync smoke)"

.PHONY: bootstrap
bootstrap:  ## Fresh box: install, then configure (CHECK=1 = check only)
	@command -v $(GO) >/dev/null 2>&1 || { echo "go not found on PATH — install the Go toolchain first"; exit 1; }
	@$(MAKE) install
	@$(if $(filter 1,$(CHECK)),$(BIN) configure --check,$(BIN) configure)

.PHONY: setup-env
setup-env:  ## Print the PATH line (APPLY=1 writes it to the shell rc)
	@line='export PATH="$$HOME/.claude/bin:$$HOME/.local/bin:$$PATH"'; \
	echo "$$line"; \
	if [ -n "$(APPLY)" ]; then \
		rc="$$HOME/.bashrc"; case "$${SHELL:-}" in */zsh) rc="$$HOME/.zshrc";; esac; \
		if grep -qxF "$$line" "$$rc" 2>/dev/null; then \
			echo -e "$(C_NOTE)already present in $$rc"; \
		else \
			printf '\n# claude-code: ~/.claude/bin + ~/.local/bin on PATH\n%s\n' "$$line" >> "$$rc"; \
			echo -e "$(C_OK)appended to $$rc"; \
		fi; \
	fi

##@ Quality
.PHONY: fmt
fmt:  ## gofmt -w every Go file
	$(GO) fmt $(PKGS) >/dev/null
	@echo -e "$(C_OK)fmt applied (gofmt)"

.PHONY: lint
lint: lint-go lint-shell lint-settings  ## gofmt, vet, shellcheck, generate --check
	@echo -e "$(C_OK)lint passed"

.PHONY: lint-go
lint-go:  # (folded into `lint`) gofmt -l + go vet
	@out="$$(gofmt -l $(GO_FMT_DIRS))"; if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi
	$(GO) vet $(PKGS)
	@echo -e "$(C_OK)gofmt + vet clean"

.PHONY: lint-shell
lint-shell:  # (folded into `lint`) shellcheck -S error over repo-owned shell scripts, if any exist
	@if [ -z "$(GO_SH_SRC)" ]; then echo -e "$(C_NOTE)no shell scripts to lint"; \
	elif command -v shellcheck >/dev/null 2>&1; then shellcheck -S error $(GO_SH_SRC) && echo -e "$(C_OK)shellcheck clean"; \
	else echo -e "$(C_NOTE)shellcheck not on PATH — skipped"; fi

.PHONY: lint-settings
lint-settings: compile  # (folded into `lint`) vendor-config + AGENTS.md/GEMINI.md render drift, against THIS checkout
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
statusline: compile  ## Render the statusline on a sample payload (COLS=…)
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
