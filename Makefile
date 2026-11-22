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
# GO_VERSION follows go.mod's `go` line, so a bootstrap install matches the module's toolchain.
GO_LOCAL_ROOT   ?= $(HOME)/.local/go
GO              ?= $(shell command -v go 2>/dev/null || echo $(GO_LOCAL_ROOT)/bin/go)
GO_VERSION      := $(shell awk '/^go /{print $$2; exit}' go.mod)
# Recipes see the local toolchain and, as a fallback only, GOPATH/bin: a stale tool there must not shadow the caller's.
export PATH     := $(GO_LOCAL_ROOT)/bin:$(PATH):$(HOME)/go/bin
BINARY          := claude-code
BIN             := bin/$(BINARY)
HOOK_BIN         = $(dir $(BIN))harness-hook
STAMP_FILE       = $(dir $(BIN)).build-stamp
PKGS            := ./...
# Dirs that `make fmt` and lint run gofmt over.
GO_FMT_DIRS     := cmd internal
GO_TEST_TIMEOUT ?= 300s
# Gate tools are PINNED: `@latest` could change under us between runs. `make install` installs all three (DEV_TOOLS=0 skips).
DEV_TOOLS       ?= 1
DEV_TOOLS_ON    := $(findstring $(DEV_TOOLS),1)
GOVULN_VERSION  ?= v1.8.0
GOLANGCI_VERSION ?= v2.15.0
YAMLFMT_VERSION ?= v0.21.0
# Markdown linter/fixer, installed to bin/ by `make install`; its pkg/markdown is the Go code's parser too.
MDSMITH_VERSION ?= v0.57.0
MDSMITH         := bin/mdsmith
RTK_VERSION     ?= 1.1.0
# TypeScript compiler for the mods under mods/ (`tsc -p mods/<name>`); `make toolchain` installs it.
TS_VERSION      ?= 7.0.2
TS_LOCAL_PREFIX ?= $(HOME)/.local
YAML_FILES      := $(shell git ls-files '*.yml' '*.yaml' 2>/dev/null)

.DEFAULT_GOAL := help

##@ Go
.PHONY: compile
compile:  ## Compile bin/claude-code and bin/harness-hook, no install
	@mkdir -p $(dir $(BIN))
	@stamp="$$(git rev-parse --short=12 HEAD 2>/dev/null || echo nogit)-$$(date +%s)"; \
	ldflags="-s -w -X github.com/edotau/claude-code/internal/paths.BuildStamp=$$stamp"; \
	$(GO) build -trimpath -ldflags="$$ldflags" -o "$(BIN).new" ./cmd/claude-code && \
	$(GO) build -trimpath -ldflags="$$ldflags" -o "$(HOOK_BIN).new" ./cmd/harness-hook && \
	rm -f "$(STAMP_FILE)" && \
	mv -f "$(HOOK_BIN).new" "$(HOOK_BIN)" && \
	mv -f "$(BIN).new" "$(BIN)" && \
	printf '%s\n' "$$stamp" > "$(STAMP_FILE).new" && \
	mv -f "$(STAMP_FILE).new" "$(STAMP_FILE)"

.PHONY: build
build: install  # (dev recipe, hidden from help) compile + install + fmt + audit (SKIP_AUDIT=1)
	@$(MAKE) --no-print-directory fmt
	@$(if $(SKIP_AUDIT),echo -e "$(C_NOTE)audit skipped (SKIP_AUDIT)")
	@$(if $(SKIP_AUDIT),,$(MAKE) audit)

# `claude-code install` links the bare shims, seeds a missing settings.json and writes bash completion (helptext.Install).
.PHONY: check-clean
check-clean:
	@status="$$(git status --porcelain --untracked-files=no)"; \
	if [ "$(FORCE)" != 1 ] && [ -n "$$status" ]; then \
		echo "Refusing to build the installed binary from a dirty tree; commit changes or use FORCE=1." >&2; exit 1; fi

.PHONY: install
install: check-clean  ## Build, install, add missing CLIs (TOOLS=1 updates all)
	@$(MAKE) compile mdsmith rtk
	@$(BIN) install$(if $(filter 1,$(V)), --verbose,)
	@$(if $(TOOLS),$(BIN) install --tools$(if $(filter-out 1,$(TOOLS)),=$(TOOLS)) && $(BIN) doctor,\
		$(BIN) install --tools --missing || echo -e "$(C_NOTE)a missing CLI failed to install (install itself succeeded)")
	@$(if $(DEV_TOOLS_ON),$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULN_VERSION) && \
		$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) && \
		$(GO) install github.com/google/yamlfmt/cmd/yamlfmt@$(YAMLFMT_VERSION) || \
		echo -e "$(C_NOTE)a dev tool failed to install (install itself succeeded)")
	@$(if $(DEV_TOOLS_ON),$(MAKE) --no-print-directory tools-sync)
	@$(BIN) theme apply --only opencode-themes >/dev/null || echo -e "$(C_NOTE)opencode theme not applied (install itself succeeded)"
	@if $(BIN) gateway proxy --restart --if-changed --force --dry-run | grep -q 'would restart'; then \
		echo -e "$(C_NOTE)the running proxy is an older build; restart it when idle: claude-code gateway proxy --restart"; fi

# A stale copy earlier on PATH (e.g. ~/.local/bin) shadows the fresh GOBIN build; after a toolchain bump it
# fails typecheck with "export data version N is greater than maximum supported" — overwrite it in place.
# A shim that resolves to another binary (mise) already runs the GOBIN build: never copy over it.
.PHONY: tools-sync
tools-sync:  ## Refresh stale PATH copies of the pinned tools from GOBIN
	@gobin="$$($(GO) env GOBIN)"; [ -n "$$gobin" ] || gobin="$$($(GO) env GOPATH)/bin"; \
	login="$$PATH"; while [ "$${login%:$(HOME)/go/bin}" != "$$login" ]; do login="$${login%:$(HOME)/go/bin}"; done; \
	for t in govulncheck golangci-lint yamlfmt; do \
		found="$$(PATH="$$login" command -v "$$t" 2>/dev/null)" || { \
			[ -x "$$gobin/$$t" ] && [ -d "$(HOME)/.local/bin" ] && cp -f "$$gobin/$$t" "$(HOME)/.local/bin/$$t" && \
			echo -e "$(C_OK)tools-sync: $$t was off the login PATH; copied to ~/.local/bin"; continue; }; \
		{ [ -x "$$gobin/$$t" ] && [ "$$found" != "$$gobin/$$t" ]; } || continue; \
		[ "$$(basename "$$(realpath "$$found")")" = "$$t" ] || continue; \
		cmp -s "$$found" "$$gobin/$$t" && continue; \
		cp -f "$$gobin/$$t" "$$found" && echo -e "$(C_OK)tools-sync: refreshed stale $$found from $$gobin/$$t"; \
	done

.PHONY: clean
clean:  ## Remove bin/claude-code and bin/harness-hook
	@rm -f "$(BIN)" "$(HOOK_BIN)" "$(STAMP_FILE)"
	@echo -e "$(C_OK)cleaned: $(BIN) $(HOOK_BIN) $(STAMP_FILE)"

.PHONY: sweep
sweep:  ## Preview stale state; APPLY=1 removes (AGE=7d, ONLY=x)
	@$(BIN) clean $(if $(APPLY),,--dry-run) $(if $(AGE),--older-than $(AGE)) $(if $(ONLY),--target $(ONLY))

.PHONY: cover
cover:  ## Per-package test coverage (opt-in; cached when unchanged)
	@set -o pipefail; $(GO) test -cover $(PKGS) | awk '/coverage:/ {print $$2, $$(NF-2)}' | sort -t' ' -k2 -n

##@ Setup
.PHONY: help-svg
help-svg: BIN := bin/.lint/$(BINARY)
help-svg: compile  ## Redraw the README menu image
	$(BIN) help-svg --out docs/images/cli-menu.svg
	@echo -e "$(C_OK)menu image regenerated; review git diff docs/"

.PHONY: bootstrap
bootstrap:  ## Set up a fresh box: toolchain, install, configure
	@$(MAKE) toolchain
	@$(MAKE) install
	@$(if $(filter 1,$(CHECK)),$(BIN) configure --check,$(BIN) configure)

INTO ?= $(HOME)/.claude
# Not spelled $(MAKE), so `make -n adopt` prints the bootstrap line instead of entering a destination not adopted yet.
ADOPT_MAKE = $(MAKE)
.PHONY: adopt
adopt:  ## Stage clone: adopt INTO (~/.claude), then bootstrap
	@$(if $(shell [ -e "$(INTO)/.git" ] && echo 1),echo "$(INTO) is already a checkout")
	@$(if $(shell [ -e "$(INTO)/.git" ] && echo 1),,$(MAKE) toolchain compile)
	@$(if $(shell [ -e "$(INTO)/.git" ] && echo 1),,CLAUDE_CONFIG_DIR="$(INTO)" $(BIN) install --setup adopt --from "$(CURDIR)" --into "$(INTO)" $(ADOPT_ARGS))
	@$(if $(or $(shell [ -e "$(INTO)/.git" ] && echo 1),$(findstring --dry-run,$(ADOPT_ARGS))),,$(ADOPT_MAKE) -C "$(INTO)" bootstrap)

.PHONY: toolchain
toolchain:  ## Install go.mod's Go and pinned tsc to ~/.local if missing
	@if command -v go >/dev/null 2>&1; then echo -e "$(C_OK)go found: $$(go version) ($$(command -v go))"; else \
	os=$$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$$(uname -m); \
	case "$$arch" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; esac; \
	command -v curl >/dev/null 2>&1 || { echo "toolchain: curl not found; install curl and re-run" >&2; exit 1; }; \
	[ ! -e "$(GO_LOCAL_ROOT)" ] || { echo "toolchain: $(GO_LOCAL_ROOT) exists but has no go on PATH; remove it and re-run" >&2; exit 1; }; \
	tgz="go$(GO_VERSION).$$os-$$arch.tar.gz"; url="https://dl.google.com/go/$$tgz"; tmp=$$(mktemp -d) || { echo "toolchain: mktemp failed" >&2; exit 1; }; \
	fail() { echo "toolchain: $$1" >&2; rm -rf "$$tmp"; exit 1; }; \
	echo "downloading $$url"; \
	curl -fsSL -o "$$tmp/$$tgz" "$$url" || fail "download failed: $$url"; \
	curl -fsSL -o "$$tmp/$$tgz.sha256" "$$url.sha256" || fail "checksum download failed: $$url.sha256"; \
	got=$$( (sha256sum "$$tmp/$$tgz" 2>/dev/null || shasum -a 256 "$$tmp/$$tgz") | cut -d' ' -f1 ); \
	[ "$$got" = "$$(cat "$$tmp/$$tgz.sha256")" ] || fail "sha256 mismatch for $$tgz"; \
	tar -C "$$tmp" -xzf "$$tmp/$$tgz" || fail "extract failed: $$tgz"; \
	mkdir -p "$(dir $(GO_LOCAL_ROOT))" || fail "cannot create $(dir $(GO_LOCAL_ROOT))"; \
	mv "$$tmp/go" "$(GO_LOCAL_ROOT)" || fail "cannot move go to $(GO_LOCAL_ROOT)"; \
	rm -rf "$$tmp"; \
	"$(GO_LOCAL_ROOT)/bin/go" version >/dev/null 2>&1 || { echo "toolchain: $(GO_LOCAL_ROOT)/bin/go does not run" >&2; exit 1; }; \
	echo -e "$(C_OK)installed $$($(GO_LOCAL_ROOT)/bin/go version) -> $(GO_LOCAL_ROOT)"; \
	echo -e "$(C_NOTE)add it to your shell: make setup-env APPLY=1"; fi; \
	if command -v tsc >/dev/null 2>&1; then echo -e "$(C_OK)tsc found: $$(tsc --version) ($$(command -v tsc))"; \
	elif ! command -v npm >/dev/null 2>&1; then echo -e "$(C_NOTE)tsc skipped: npm not found (it ships as an npm package)"; \
	else npm install --global --prefix "$(TS_LOCAL_PREFIX)" --no-fund --no-audit "typescript@$(TS_VERSION)" && \
	echo -e "$(C_OK)installed $$($(TS_LOCAL_PREFIX)/bin/tsc --version) -> $(TS_LOCAL_PREFIX)/bin/tsc"; fi

.PHONY: setup-env
setup-env:  ## Print the PATH line (APPLY=1 writes shell rc)
	@d="$(CURDIR)"; [ "$$d" = "$$HOME/.claude" ] && d='$$HOME/.claude'; \
	line="export PATH=\"$$d/bin:\$$HOME/.local/bin:\$$HOME/.local/go/bin:\$$HOME/go/bin:\$$PATH\""; \
	echo "$$line"; \
	if [ -n "$(APPLY)" ]; then \
		rc="$$HOME/.bashrc"; case "$${SHELL:-}" in */zsh) rc="$$HOME/.zshrc";; esac; \
		if grep -qxF "$$line" "$$rc" 2>/dev/null; then \
			echo -e "$(C_NOTE)already present in $$rc"; \
		else \
			printf '\n# claude-code: %s/bin, ~/.local/bin, ~/.local/go/bin, ~/go/bin on PATH\n%s\n' "$$d" "$$line" >> "$$rc"; \
			echo -e "$(C_OK)appended to $$rc"; \
		fi; \
	fi

##@ Quality
# Umbrella gate on the working-tree build (bin/.lint, never the live binary): lint, test and the read-only checks run in
# parallel with one log per stage; the writer (agents sync) runs alone afterwards. Compile + vet cover the build.
# Each worker runs under `set -m` (own process group) so a cancel signals `-pid` and reaches its grandchildren.
# AUDIT_INNER=1 drops the audit-* targets' `compile` prerequisite: audit compiles once, standalone runs compile themselves.
AUDIT_LINT   ?= $(MAKE) lint SKIP=docs
AUDIT_TEST   ?= if [ "$(SKIP_TESTS)" = 1 ]; then echo -e "$(C_NOTE)tests skipped (SKIP_TESTS=1)"; else $(BIN) test ramdisk -- $(MAKE) audit-test; fi
AUDIT_CHECKS ?= $(MAKE) -k audit-git audit-vuln audit-prompts audit-svg BIN=$(BIN) AUDIT_INNER=1
AUDIT_PRE    := $(if $(AUDIT_INNER),,compile)
.PHONY: audit
audit: BIN := bin/.lint/$(BINARY)
audit: check-clean compile  ## Local gate: lint, git secrets, test, build, vuln, docs
	@set -euo pipefail; d=$$(mktemp -d); c=0; rc=0; pids=(); names=(lint test checks); set -m; \
	trap 'rm -r -f -- "$$d"' EXIT; \
	stop() { c=$$1; for p in $${pids[@]+"$${pids[@]}"}; do kill -TERM -- -$$p 2>/dev/null || :; done; }; \
	trap 'stop 130' INT; trap 'stop 143' TERM; \
	( $(AUDIT_LINT) ) >"$$d/lint" 2>&1 </dev/null & pids+=($$!); \
	( $(AUDIT_TEST) ) >"$$d/test" 2>&1 </dev/null & pids+=($$!); \
	( $(AUDIT_CHECKS) ) >"$$d/checks" 2>&1 </dev/null & pids+=($$!); set +m; \
	for i in 0 1 2; do n=$${names[$$i]}; if wait $${pids[$$i]}; then s=ok; else s=FAILED; fi; \
		if [ $$c != 0 ]; then wait $${pids[$$i]} 2>/dev/null || :; s=cancelled; fi; \
		[ $$s = ok ] || rc=1; echo "== $$n: $$s =="; cat "$$d/$$n" || rc=1; done; \
	[ $$c = 0 ] || { echo "audit cancelled" >&2; exit $$c; }; \
	[ $$rc = 0 ] || { echo "audit failed: see the FAILED stages above" >&2; exit 1; }
	@$(MAKE) lint-docs CHECK=1
	@$(MAKE) audit-sync BIN=$(BIN) AUDIT_INNER=1
	@echo -e "$(C_OK)audit passed (lint/test/build + sync smoke + generated docs; vuln when installed and online)"

audit-git audit-prompts audit-svg audit-sync: BIN := bin/.lint/$(BINARY)
audit-git audit-prompts audit-svg audit-sync: $(AUDIT_PRE)

.PHONY: audit-vuln
audit-vuln:  # (folded into `audit`) govulncheck when installed and vuln.go.dev answers
	@if ! command -v govulncheck >/dev/null 2>&1; then echo -e "$(C_NOTE)govulncheck not installed — skipped (make install)"; \
		elif ! curl -fsS -m 5 -o /dev/null https://vuln.go.dev/index/db.json 2>/dev/null; then \
			echo -e "$(C_NOTE)govulncheck skipped (vuln.go.dev unreachable)"; \
		else govulncheck $(PKGS); fi

.PHONY: audit-prompts
audit-prompts:  # (folded into `audit`) doctor --prompts
	@$(BIN) doctor --prompts

.PHONY: audit-sync
audit-sync:  # (folded into `audit`) agents sync --dry-run; drift runs the real sync
	@out="$$(CLAUDE_CONFIG_DIR=$(CURDIR) $(BIN) agents sync --dry-run 2>&1)" || { echo "$$out"; echo -e "$(C_NOTE)sync drift: running agents sync"; \
		CLAUDE_CONFIG_DIR=$(CURDIR) $(BIN) agents sync || exit 1; }

.PHONY: audit-svg
audit-svg:  # (folded into `audit`) docs/images/cli-menu.svg matches the help-svg render
	@t=$$(mktemp); rc=0; $(BIN) help-svg --out $$t && cmp -s $$t docs/images/cli-menu.svg || rc=1; rm -f $$t; \
		if [ $$rc -ne 0 ]; then echo "docs/images/cli-menu.svg is stale: run make help-svg"; exit 1; fi

# Fast tier: skips the pre-ship tests (`testing.Short`); `audit` still runs everything.
.PHONY: test
test:
	@$(GO) test -short -timeout $(GO_TEST_TIMEOUT) $(PKGS)

# One `go test` at a time: it already spreads packages over the cores; seven at once oversubscribed them and made
# wall-clock tests flake. Measured the same (31s vs 33s sharded); strictly serial (-p 1) took 168s.
TEST_PKGS ?= $(PKGS)
.PHONY: audit-test
audit-test:  # (folded into `audit`) go test TEST_PKGS in one invocation
	@$(GO) test -timeout $(GO_TEST_TIMEOUT) $(TEST_PKGS)

# Commits being pushed: AUDIT_GIT_RANGE, else @{u}..HEAD, else the last AUDIT_GIT_FALLBACK commits.
AUDIT_GIT_FALLBACK ?= 20
.PHONY: audit-git
audit-git:  # (folded into `audit`) doctor --git over the unpushed commits; a token-shaped addition fails
	@if [ -n "$(AUDIT_GIT_RANGE)" ]; then exec $(BIN) doctor --git --range '$(AUDIT_GIT_RANGE)'; fi; \
		if ! n=$$(git rev-list --count '@{u}..HEAD' 2>/dev/null); then exec $(BIN) doctor --git --since $(AUDIT_GIT_FALLBACK); fi; \
		if [ "$$n" = 0 ]; then echo -e "$(C_NOTE)doctor --git skipped (no unpushed commits)"; exit 0; fi; \
		$(BIN) doctor --git --range '@{u}..HEAD'

# One lint per kind, each a branch of lint-% below; `make lint-<kind>` runs one.
LINT_WHAT := go quality shell yaml help docs js settings

WHAT    ?= all
SKIP    ?=
C_COMMA := ,
LINT_KINDS := $(filter-out $(subst $(C_COMMA), ,$(SKIP)),$(or $(if $(filter go,$(WHAT)),go quality),$(if $(filter md,$(WHAT)),docs),$(filter $(WHAT),js settings),$(LINT_WHAT)))
FMT_KINDS := $(filter go yaml docs $(filter settings,$(WHAT)),$(LINT_KINDS))
.PHONY: fmt
fmt:  ## Fix go, yaml, md, memory banks; then show what lint finds
	@case "$(WHAT)" in go|md|js|settings|all) ;; *) echo "WHAT must be go|md|js|settings|all"; exit 1 ;; esac
	$(GO) fmt $(PKGS) >/dev/null
	@if command -v golangci-lint >/dev/null; then golangci-lint fmt $(GO_FMT_DIRS); fi
	@$(if $(FMT_KINDS),$(MAKE) --no-print-directory -k $(addprefix lint-,$(FMT_KINDS)) CHECK= || :)
	@echo -e "$(C_OK)fmt applied; what lint still reports needs a hand fix"
	@$(MAKE) --no-print-directory lint

.PHONY: lint
lint:  ## Check only, never writes: go, shell, yaml, md, js, memory
	@case "$(WHAT)" in go|md|js|settings|all) ;; *) echo "WHAT must be go|md|js|settings|all"; exit 1 ;; esac
	@$(if $(SKIP),echo -e "$(C_NOTE)lint skipping: $(SKIP)")
	@$(MAKE) --no-print-directory -k $(addprefix lint-,$(LINT_KINDS)) CHECK=1
	@echo -e "$(C_OK)lint passed"

lint-%: BIN := bin/.lint/$(BINARY)
lint-%: compile mdsmith  # (helper) one lint kind: $(LINT_WHAT); CHECK=1 reports, else fixes (go: gofmt -w, golangci --fix)
	@case "$*" in \
	go) if [ "$(CHECK)" = 1 ]; then g=gofmt,vet; \
			if command -v golangci-lint >/dev/null; then g=$$g,golangci-lint; \
			else echo -e "$(C_NOTE)golangci-lint skipped: not installed (make install DEV_TOOLS=1)"; fi; \
			$(BIN) agents diff --check --all --gates $$g; \
		else gofmt -w $(GO_FMT_DIRS); \
			if command -v golangci-lint >/dev/null; then \
				golangci-lint run --fix --disable gocognit --max-issues-per-linter=0 --max-same-issues=0 ./... || :; \
			else echo -e "$(C_NOTE)golangci-lint skipped: not installed (make install DEV_TOOLS=1)"; fi; fi ;; \
	quality) $(BIN) review simplify --fail ;; \
	shell) $(BIN) agents diff --check --all --gates shell ;; \
	yaml) if [ "$(CHECK)" = 1 ]; then $(BIN) agents diff --check --all --gates yaml; \
		elif ! command -v yamlfmt >/dev/null 2>&1; then echo -e "$(C_NOTE)yamlfmt not installed — skipped (make install)"; \
		elif [ -n "$(YAML_FILES)" ]; then yamlfmt $(YAML_FILES); fi ;; \
	help) ;; \
	docs) if [ "$(CHECK)" = 1 ]; then $(BIN) agents diff --check --all --gates docs; else rc=0; $(MDSMITH) fix || rc=1; \
		live="$${CLAUDE_CONFIG_DIR:-$$HOME/.claude}"; \
		if [ "$$(realpath "$(CURDIR)" 2>/dev/null)" = "$$(realpath "$$live" 2>/dev/null)" ]; then \
			$(BIN) memory fmt || rc=1; \
		else echo -e "$(C_NOTE)memory fmt skipped: $(CURDIR) is not the live config dir ($$live) — a worktree must not write the shared memory store"; fi; \
		exit $$rc; fi ;; \
	js) $(BIN) workflow lint ;; \
	settings) $(BIN) configure --fmt $(if $(filter 1,$(CHECK)),--check) ;; \
	*) printf "Unknown lint '%s' (use %s)\n" "$*" "$(subst $(C_SP),|,$(LINT_WHAT))"; exit 1 ;; \
	esac
	@$(if $(filter help,$*),$(MAKE) --no-print-directory help-check)

# Help rows must fit 80 columns; its recipe holds no $(MAKE), so `make -n` never starts the pipeline.
.PHONY: help-check
help-check:
	@set -o pipefail; out="$$($(HELP_TEXT) | sed 's/\x1b\[[0-9;]*m//g' | awk \
			'length>80 && substr($$0,20) ~ / / {print "over 80: " $$0} /^   / && !/^                   [^ ]/ {print "misaligned continuation: " $$0}')" || { echo "make help failed"; exit 1; }; \
		if [ -n "$$out" ]; then echo "help rows break the wrap rule:"; echo "$$out"; exit 1; fi

.PHONY: mdsmith
mdsmith:  # (folded into `install`) pinned mdsmith binary in bin/
	@if ! $(MDSMITH) version 2>/dev/null | grep -qF '$(MDSMITH_VERSION)'; then \
		GOBIN=$(CURDIR)/bin $(GO) install github.com/jeduden/mdsmith/cmd/mdsmith@$(MDSMITH_VERSION); fi

.PHONY: rtk
rtk:  # (folded into `install`) pinned rtk release in bin/; a warning, never a failure
	@if bin/rtk --version 2>/dev/null | grep -qF ' $(RTK_VERSION)'; then exit 0; fi; \
	case "$$(uname -s)-$$(uname -m)" in \
		Linux-x86_64) a=x86_64-unknown-linux-musl;; Linux-aarch64|Linux-arm64) a=aarch64-unknown-linux-gnu;; \
		Darwin-x86_64) a=x86_64-apple-darwin;; Darwin-arm64) a=aarch64-apple-darwin;; \
		*) echo -e "$(C_NOTE)rtk skipped: unsupported platform $$(uname -s)-$$(uname -m)"; exit 0;; esac; \
	sum() { if command -v sha256sum >/dev/null; then sha256sum "$$1"; else shasum -a 256 "$$1"; fi; }; \
	u=https://github.com/rtk-ai/rtk/releases/download/v$(RTK_VERSION); t=$$(mktemp -d); \
	if curl -fsSL -o $$t/rtk.tgz $$u/rtk-$$a.tar.gz && curl -fsSL -o $$t/checksums.txt $$u/checksums.txt \
		&& [ "$$(sum $$t/rtk.tgz | cut -d' ' -f1)" = "$$(grep -F "rtk-$$a.tar.gz" $$t/checksums.txt | cut -d' ' -f1)" ] \
		&& tar -xzf $$t/rtk.tgz -C $$t rtk && mkdir -p bin && install -m 0755 $$t/rtk bin/rtk; then \
		echo -e "$(C_OK)rtk $(RTK_VERSION) installed (checksum verified)"; \
	else echo -e "$(C_NOTE)rtk $(RTK_VERSION) skipped: download or checksum failed"; fi; rm -rf $$t

.PHONY: verify
verify: audit  ## audit + doctor + proxy status
	@$(BIN) doctor
	@$(BIN) gateway proxy --status
	@echo -e "$(C_OK)verify passed (audit + doctor + proxy status)"

##@ Container
TARGET ?= nested-podman
IMAGE ?= claude-code-harness:$(TARGET)
.PHONY: image image-audit image-shell
image:  ## Ubuntu image of non-ignored files (TARGET=base|nested-podman)
	TARGET=$(TARGET) IMAGE=$(IMAGE) container/image.sh build

image-audit: image  ## Run make audit inside a fresh Ubuntu container
	TARGET=$(TARGET) IMAGE=$(IMAGE) container/image.sh run make audit

image-shell:  ## Open a shell in the built image (no rebuild)
	TARGET=$(TARGET) IMAGE=$(IMAGE) container/image.sh run bash

##@ Diagnostics
.PHONY: statusline
statusline: BIN := bin/.lint/$(BINARY)
statusline: compile  ## Render a sample statusline (COLS=…)
	@echo '{"model":{"id":"claude-sonnet-5","display_name":"Sonnet 5"},"workspace":{"current_dir":"'$(CURDIR)'"},"transcript_path":"/tmp/x","context_window":{"context_window_size":1000000,"used_percentage":42}}' \
		| COLUMNS=$${COLS:-120} $(BIN) theme statusline

##@ Quick Reference
# The help text, shared by `help` and `help-check`: one shell group, every line continued.
define HELP_TEXT
{ printf "\n\033[1mclaude-code\033[0m\n"; \
printf "\033[2mUsage:\033[0m make <target> [VAR=value]\n"; \
awk 'BEGIN{FS=":"} \
	/^##@/{sec=substr($$0,5); if(!(sec in o)) o[sec]=++n; next} \
	/^[a-zA-Z0-9_.%\/-]+:[^=]/{t=$$1; if(t ~ /^[._]/) next; \
		if(!match($$0,/##[ \t]*/)) next; d=substr($$0,RSTART+RLENGTH); \
		if(t in seen) next; seen[t]=1; \
		printf "%d\t%s\t%s\t%s\n", o[sec], sec, t, d}' $(MAKEFILE_LIST) \
	| sort -t "$$(printf '\t')" -k1,1n -k3,3 \
	| awk -F"\t" '{if($$2!=cur){cur=$$2; printf "\n\033[1m%s\033[0m\n", $$2} \
		n=split($$4,w," "); line=""; lab=sprintf("  \033[36m%-16s\033[0m ",$$3); \
		for(i=1;i<=n;i++){ if(line!="" && 19+length(line)+1+length(w[i])>80){ \
			print lab line; lab=sprintf("%19s",""); line=w[i]} \
			else line=(line==""?w[i]:line" "w[i]) } \
		print lab line}'; \
printf "\n"; }
endef
.PHONY: help
help:  ## Show this help
	@$(HELP_TEXT)
