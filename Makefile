# =============================================================================
# ENVIRONMENT
# =============================================================================
SHELL           := /bin/bash
.SHELLFLAGS     := -euo pipefail -c
MAKEFLAGS       += --no-print-directory

C_EMPTY :=
C_SP    := $(C_EMPTY) $(C_EMPTY)
ifdef NO_COLOR
C_OK   := OK$(C_SP)
C_NOTE := note:$(C_SP)
else
C_OK   := \033[1;32mOK\033[0m$(C_SP)
C_NOTE := \033[33mnote:\033[0m$(C_SP)
endif

# A `go` on PATH wins; else ~/.local/go at go.mod's version. GOPATH/bin goes last so a stale tool there never shadows.
GO_LOCAL_ROOT   ?= $(HOME)/.local/go
GO              ?= $(shell command -v go 2>/dev/null || echo $(GO_LOCAL_ROOT)/bin/go)
GO_VERSION      := $(shell awk '/^go /{print $$2; exit}' go.mod)
export PATH     := $(GO_LOCAL_ROOT)/bin:$(PATH):$(HOME)/go/bin

BINARY          := claude-code
BIN             := bin/$(BINARY)
LINT_BIN        := bin/.lint/$(BINARY)# working-tree build for gates; never the live binary
HOOK_BIN         = $(dir $(BIN)).hookd
HOOK_BIN_MAX     = 28311552# 27 MiB: SQLite in the hook; linux/amd64 builds it at 26.1 MB, darwin/arm64 at 24.6 MB
STAMP_FILE       = $(dir $(BIN)).build-stamp
RTKWRAP_BIN      = $(dir $(BIN)).rtkwrap
PKGS            := ./...
GO_FMT_DIRS     := cmd internal
GO_TEST_TIMEOUT ?= 300s
YAML_FILES      := $(shell git ls-files '*.yml' '*.yaml' 2>/dev/null)

# Pinned tool versions: `@latest` could change between runs. DEV_TOOLS=0 makes install skip the Go dev tools.
DEV_TOOLS       ?= 1
DEV_TOOLS_ON    := $(findstring $(DEV_TOOLS),1)
GOVULN_VERSION  ?= v1.8.0
GOLANGCI_VERSION ?= v2.15.0
YAMLFMT_VERSION ?= v0.21.0
BENCHSTAT_VERSION ?= v0.0.0-20261117185222-4febdb082b2c
MDSMITH_VERSION ?= v0.57.0
MDSMITH         := bin/mdsmith
MARKDOWNLINT_VERSION ?= 0.24.2# matches VS Code's markdownlint extension bundle
RTK_VERSION     ?= 1.1.0
TYPOS_VERSION   := v1.52.2
TS_VERSION      ?= 7.0.2
TS_LOCAL_PREFIX ?= $(HOME)/.local

# Shell snippets shared by recipes.
SHA256       = sha256() { if command -v sha256sum >/dev/null; then sha256sum "$$1"; else shasum -a 256 "$$1"; fi | cut -d' ' -f1; };
NO_GOLANGCI  = echo -e "$(C_NOTE)golangci-lint skipped: not installed (make install DEV_TOOLS=1)"
CHECK_WHAT   = case "$(WHAT)" in go|md|settings|all) ;; *) echo "WHAT must be go|md|settings|all"; exit 1 ;; esac

# =============================================================================
# HELP
# =============================================================================
.DEFAULT_GOAL := help

# Groups are the banner titles in file order (HELP last); targets keep file order; rows wrap at 80 columns.
define HELP_TEXT
{ printf "\n\033[1mclaude-code\033[0m\n"; \
printf "\033[2mUsage:\033[0m make <target> [VAR=value]\n"; \
awk 'BEGIN{FS=":"} \
	function tc(s,  n,w,i,o){n=split(tolower(s),w," "); for(i=1;i<=n;i++) o=o (i>1?" ":"") toupper(substr(w[i],1,1)) substr(w[i],2); return o} \
	/^# =+$$/{if(r==NR-2 && p ~ /^# /){sec=tc(substr(p,3)); if(!(sec in o)) o[sec]=(sec=="Help"?999:++n)} r=NR; next} \
	{p=$$0} \
	/^[a-zA-Z0-9_.%\/-]+:[^=]/{t=$$1; if(t ~ /^[._]/) next; \
		if(!match($$0,/##[ \t]*/)) next; d=substr($$0,RSTART+RLENGTH); \
		if(t in seen) next; seen[t]=1; \
		printf "%d\t%06d\t%s\t%s\t%s\n", o[sec], NR, sec, t, d}' $(MAKEFILE_LIST) \
	| sort -t "$$(printf '\t')" -k1,1n -k2,2n \
	| awk -F"\t" '{if($$3!=cur){cur=$$3; printf "\n\033[1m%s\033[0m\n", $$3} \
		n=split($$5,w," "); line=""; lab=sprintf("  \033[36m%-16s\033[0m ",$$4); \
		for(i=1;i<=n;i++){ if(line!="" && 19+length(line)+1+length(w[i])>80){ \
			print lab line; lab=sprintf("%19s",""); line=w[i]} \
			else line=(line==""?w[i]:line" "w[i]) } \
		print lab line}'; \
printf "\n\033[2mTip:\033[0m make list prints every target, one per line.\n\n"; }
endef

.PHONY: help list help-check
help:  ## Show this help
	@$(HELP_TEXT)

list:  ## Every target, one per line (scripts, completion)
	@awk -F: '/^[a-zA-Z0-9_.%\/-]+:[^=]/ && $$1 !~ /^[._]/ {print $$1}' $(MAKEFILE_LIST) | sort -u

# No $(MAKE) in this recipe, so `make -n` never starts the pipeline.
help-check:  # (lint-help) help rows fit 80 columns and continuations align
	@set -o pipefail; out="$$($(HELP_TEXT) | sed 's/\x1b\[[0-9;]*m//g' | awk \
			'length>80 && substr($$0,20) ~ / / {print "over 80: " $$0} /^   / && !/^                   [^ ]/ {print "misaligned continuation: " $$0}')" || { echo "make help failed"; exit 1; }; \
		if [ -n "$$out" ]; then echo "help rows break the wrap rule:"; echo "$$out"; exit 1; fi

# =============================================================================
# BUILD & INSTALL
# =============================================================================
# macOS: a stable signing identity keeps the LaunchAgents' binary "the same" across rebuilds, so Background Items
# stops notifying on every install; without the cert (`make codesign-cert`) the linker's ad-hoc signature stays.
CODESIGN_ID     ?= claude-code-local
sign = { [ "$$(uname)" != Darwin ] || ! security find-identity -p codesigning 2>/dev/null | grep -q '"$(CODESIGN_ID)"' || \
	codesign -f -s "$(CODESIGN_ID)" -i $(BINARY) "$(1)" 2>/dev/null || echo -e "$(C_NOTE)codesign $(1) failed; kept ad-hoc"; }

.PHONY: compile generate build install codesign-cert clean sweep tools-sync mdsmith markdownlint rtk typos
# Builds to temp names and moves into place; signing precedes the stamp, which records the final size.
compile:  ## Build the bin/ binaries; no install
	@mkdir -p $(dir $(BIN))
	@stamp="$$(git rev-parse --short=12 HEAD 2>/dev/null || echo nogit)-$$(date +%s)-$$$$"; tmp=".new.$$$$"; \
	ldflags="-s -w -X github.com/edotau/claude-code/internal/platform/paths.BuildStamp=$$stamp"; \
	$(GO) build -trimpath -ldflags="$$ldflags" -o "$(BIN)$$tmp" ./cmd/claude-code && \
	$(GO) build -trimpath -ldflags="$$ldflags" -o "$(HOOK_BIN)$$tmp" ./cmd/harness-hook && \
	size=$$(wc -c < "$(HOOK_BIN)$$tmp" | tr -d ' ') && \
	{ [ "$$size" -le $(HOOK_BIN_MAX) ] || { rm -f "$(BIN)$$tmp" "$(HOOK_BIN)$$tmp"; \
	  echo "compile: $(HOOK_BIN) is $$size bytes, over HOOK_BIN_MAX=$(HOOK_BIN_MAX): an import pulled heavy code into it" >&2; exit 1; }; } && \
	$(call sign,$(BIN)$$tmp) && \
	rm -f "$(STAMP_FILE)" && \
	mv -f "$(HOOK_BIN)$$tmp" "$(HOOK_BIN)" && \
	mv -f "$(BIN)$$tmp" "$(BIN)" && \
	printf '%s %s %s\n' "$$stamp" "$$(wc -c < "$(BIN)" | tr -d ' ')" "$$(date -r "$(BIN)" +%s)" > "$(STAMP_FILE)$$tmp" && \
	mv -f "$(STAMP_FILE)$$tmp" "$(STAMP_FILE)" && \
	$(GO) build -trimpath -ldflags="-s -w" -o "$(RTKWRAP_BIN)$$tmp" ./cmd/rtkwrap && \
	mv -f "$(RTKWRAP_BIN)$$tmp" "$(RTKWRAP_BIN)"

# Committed files that tests compare against a render; those tests fail until this runs after a change.
generate: BIN := $(LINT_BIN)
generate: compile  ## Regenerate committed files: README menu image, overlay schema, registry golden
	$(BIN) help-svg --out docs/images/cli-menu.svg
	UPDATE_OVERLAY_SCHEMA=1 $(GO) test ./internal/agentconfig -run '^TestOverlaySchemaGolden$$' >/dev/null
	$(GO) test ./internal/cli -run '^TestContractRegistryGolden$$' -update >/dev/null
	@echo -e "$(C_OK)generated files refreshed; review git diff"

build: install  # (dev) install + fmt + audit; SKIP_AUDIT=1 skips the audit
	@$(MAKE) --no-print-directory fmt
	@$(if $(SKIP_AUDIT),echo -e "$(C_NOTE)audit skipped (SKIP_AUDIT)")
	@$(if $(SKIP_AUDIT),,$(MAKE) audit)

# Optional steps after `claude-code install` only warn: install itself has already succeeded.
install:  ## Build, install, add missing CLIs (TOOLS=1 updates all)
	@$(MAKE) compile mdsmith markdownlint rtk typos
	@$(BIN) install$(if $(filter 1,$(V)), --verbose,)
	@dest="$${CLAUDE_CONFIG_DIR:-$$HOME/.claude}/bin"; mkdir -p "$$dest"; \
		if [ -f bin/typos ] && ! [ bin/typos -ef "$$dest/typos" ]; then install -m 0755 bin/typos "$$dest/typos"; fi
	@$(if $(TOOLS),$(BIN) install --tools$(if $(filter-out 1,$(TOOLS)),=$(TOOLS)) && $(BIN) doctor,\
		$(BIN) install --tools --missing || echo -e "$(C_NOTE)a missing CLI failed to install (install itself succeeded)")
	@$(if $(DEV_TOOLS_ON),$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULN_VERSION) && \
		$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) && \
		$(GO) install github.com/google/yamlfmt/cmd/yamlfmt@$(YAMLFMT_VERSION) && \
		$(GO) install golang.org/x/perf/cmd/benchstat@$(BENCHSTAT_VERSION) || \
		echo -e "$(C_NOTE)a dev tool failed to install (install itself succeeded)")
	@$(if $(DEV_TOOLS_ON),$(MAKE) --no-print-directory tools-sync)
	@$(BIN) configure theme apply --only opencode-themes >/dev/null || echo -e "$(C_NOTE)opencode theme not applied (install itself succeeded)"
	@out="$$($(BIN) gateway proxy --restart --if-changed 2>&1)"; case "$$out" in \
		*unchanged*|*"not running"*) ;; *restarted*) echo "$$out" ;; \
		*) echo "$$out"; echo -e "$(C_NOTE)proxy not replaced now; the status line or the next fenced mint replaces it" ;; esac
	@out="$$($(BIN) gateway proxy --status 2>&1 || true)"; n="$$(printf '%s\n' "$$out" | grep -c '^problem ' || true)"; t="$$(printf '%s\n' "$$out" | grep -c '^teams: ' || true)"; \
		if [ "$$n" != 0 ] || [ "$$t" = 0 ]; then echo "$$out"; exit 1; fi
	@if $(BIN) agents serve --status >/dev/null 2>&1; then \
		$(BIN) agents serve --restart || echo -e "$(C_NOTE)agents serve did not restart (install itself succeeded)"; fi

codesign-cert:  ## macOS: create the self-signed CODESIGN_ID cert in the login keychain (once per Mac)
	@if [ "$$(uname)" != Darwin ]; then echo -e "$(C_NOTE)codesign-cert is macOS-only; nothing to do"; exit 0; fi; \
	if security find-identity -p codesigning | grep -q '"$(CODESIGN_ID)"'; then echo -e "$(C_OK)$(CODESIGN_ID) exists"; exit 0; fi; \
	d="$$(mktemp -d)"; trap 'rm -rf "$$d"' EXIT; pw="$$(openssl rand -hex 16)"; \
	printf '[req]\ndistinguished_name=dn\nprompt=no\n[dn]\nCN=%s\n[ext]\nbasicConstraints=critical,CA:false\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=critical,codeSigning\n' \
		"$(CODESIGN_ID)" > "$$d/c.cnf"; \
	openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -config "$$d/c.cnf" -extensions ext \
		-keyout "$$d/k.pem" -out "$$d/c.pem" 2>/dev/null; \
	openssl pkcs12 -export -legacy -inkey "$$d/k.pem" -in "$$d/c.pem" -out "$$d/id.p12" -passout "pass:$$pw"; \
	security import "$$d/id.p12" -k "$$HOME/Library/Keychains/login.keychain-db" -P "$$pw" -T /usr/bin/codesign >/dev/null; \
	echo -e "$(C_OK)$(CODESIGN_ID) created; the next make install signs with it"

clean:  ## Remove bin/claude-code and bin/.hookd
	@rm -f "$(BIN)" "$(HOOK_BIN)" "$(dir $(BIN)).hooks" "$(STAMP_FILE)"
	@echo -e "$(C_OK)cleaned: $(BIN) $(HOOK_BIN) $(STAMP_FILE)"

sweep:  ## Remove stale state; DRY_RUN=1 previews (AGE=7d, ONLY=x)
	@$(BIN) clean $(if $(DRY_RUN),--dry-run) $(if $(AGE),--older-than $(AGE)) $(if $(ONLY),--target $(ONLY))

# A stale PATH copy (e.g. ~/.local/bin) fails typecheck after a toolchain bump; never copy over a shim (mise).
tools-sync:  ## Refresh stale PATH copies of the pinned tools from GOBIN
	@gobin="$$($(GO) env GOBIN)"; [ -n "$$gobin" ] || gobin="$$($(GO) env GOPATH)/bin"; \
	login="$$PATH"; while [ "$${login%:$(HOME)/go/bin}" != "$$login" ]; do login="$${login%:$(HOME)/go/bin}"; done; \
	for t in govulncheck golangci-lint yamlfmt benchstat; do \
		found="$$(PATH="$$login" command -v "$$t" 2>/dev/null)" || { \
			[ -x "$$gobin/$$t" ] && [ -d "$(HOME)/.local/bin" ] && cp -f "$$gobin/$$t" "$(HOME)/.local/bin/$$t" && \
			echo -e "$(C_OK)tools-sync: $$t was off the login PATH; copied to ~/.local/bin"; continue; }; \
		{ [ -x "$$gobin/$$t" ] && [ "$$found" != "$$gobin/$$t" ]; } || continue; \
		[ "$$(basename "$$(realpath "$$found")")" = "$$t" ] || continue; \
		cmp -s "$$found" "$$gobin/$$t" && continue; \
		cp -f "$$gobin/$$t" "$$found" && echo -e "$(C_OK)tools-sync: refreshed stale $$found from $$gobin/$$t"; \
	done

mdsmith:  # (install) pinned mdsmith in bin/; its pkg/markdown is also the Go code's parser
	@if ! $(MDSMITH) version 2>/dev/null | grep -qF '$(MDSMITH_VERSION)'; then \
		GOBIN=$(CURDIR)/bin $(GO) install github.com/jeduden/mdsmith/cmd/mdsmith@$(MDSMITH_VERSION); fi

markdownlint:  # (install) pinned markdownlint-cli2 under state/harness/markdownlint; warns, never fails
	@command -v npm >/dev/null 2>&1 || { echo -e "$(C_NOTE)markdownlint skipped: npm not found (it ships as an npm package)"; exit 0; }; \
		target="state/harness/markdownlint"; installed="$$target/node_modules/.bin/markdownlint-cli2"; \
		if [ -x "$$installed" ] && grep -qF '"version": "$(MARKDOWNLINT_VERSION)"' "$$target/node_modules/markdownlint-cli2/package.json" 2>/dev/null; then \
			exit 0; \
		fi; \
		mkdir -p "$$target" && \
		{ npm install --prefix "$$target" --no-fund --no-audit --save-exact "markdownlint-cli2@$(MARKDOWNLINT_VERSION)" && \
			echo -e "$(C_OK)installed markdownlint-cli2@$(MARKDOWNLINT_VERSION) -> $$target"; } || \
			echo -e "$(C_NOTE)markdownlint-cli2 install failed: npm install error"

rtk:  # (install) pinned rtk release in bin/, checked against its checksums.txt; warns, never fails
	@if bin/rtk --version 2>/dev/null | grep -qF ' $(RTK_VERSION)'; then exit 0; fi; \
	case "$$(uname -s)-$$(uname -m)" in \
		Linux-x86_64) a=x86_64-unknown-linux-musl;; Linux-aarch64|Linux-arm64) a=aarch64-unknown-linux-gnu;; \
		Darwin-x86_64) a=x86_64-apple-darwin;; Darwin-arm64) a=aarch64-apple-darwin;; \
		*) echo -e "$(C_NOTE)rtk skipped: unsupported platform $$(uname -s)-$$(uname -m)"; exit 0;; esac; \
	$(SHA256) u=https://github.com/rtk-ai/rtk/releases/download/v$(RTK_VERSION); t=$$(mktemp -d); \
	if curl -fsSL -o $$t/rtk.tgz $$u/rtk-$$a.tar.gz && curl -fsSL -o $$t/checksums.txt $$u/checksums.txt \
		&& [ "$$(sha256 $$t/rtk.tgz)" = "$$(grep -F "rtk-$$a.tar.gz" $$t/checksums.txt | cut -d' ' -f1)" ] \
		&& tar -xzf $$t/rtk.tgz -C $$t rtk && mkdir -p bin && install -m 0755 $$t/rtk bin/rtk; then \
		echo -e "$(C_OK)rtk $(RTK_VERSION) installed (checksum verified)"; \
	else echo -e "$(C_NOTE)rtk $(RTK_VERSION) skipped: download or checksum failed"; fi; rm -rf $$t

typos:  # (install) pinned typos release in bin/, checked against the sums below; warns, never fails
	@if bin/typos --version 2>/dev/null | grep -qxF 'typos-cli $(TYPOS_VERSION:v%=%)'; then exit 0; fi; \
	case "$$(uname -s)-$$(uname -m)" in \
		Darwin-arm64) a=aarch64-apple-darwin; s=b8e7b8b81f28cc808fd763b6d935536e15ba1ef72613bbb6d628b58a891da7ef;; \
		Darwin-x86_64) a=x86_64-apple-darwin; s=2c1e5cd3be644005d27543d7d83a32dbba36eb285bc12678818709938a63a988;; \
		Linux-aarch64|Linux-arm64) a=aarch64-unknown-linux-musl; s=f2f54b567f5f4688f000f1bf85c585ee22cce3b55befc9418bcdd4c5adc16595;; \
		Linux-x86_64) a=x86_64-unknown-linux-musl; s=3b13d1291281d39d01822e54035636487a7a0f228e70097899b1e33aeb0e7e93;; \
		*) echo -e "$(C_NOTE)typos skipped: unsupported platform $$(uname -s)-$$(uname -m)"; exit 0;; esac; \
	t=$$(mktemp -d "$${TMPDIR:-/tmp}/typos.XXXXXXXX"); trap 'rm -rf "$$t"' EXIT; $(SHA256) \
	u="https://github.com/crate-ci/typos/releases/download/$(TYPOS_VERSION)/typos-$(TYPOS_VERSION)-$$a.tar.gz"; \
	curl -fsSL -o "$$t/typos.tar.gz" "$$u" || { echo -e "$(C_NOTE)typos skipped: download failed"; exit 0; }; \
	[ "$$(sha256 "$$t/typos.tar.gz")" = "$$s" ] || { echo -e "$(C_NOTE)typos skipped: checksum mismatch"; exit 0; }; \
	tar -xzf "$$t/typos.tar.gz" -C "$$t" && [ -f "$$t/typos" ] || { echo -e "$(C_NOTE)typos skipped: binary not in archive"; exit 0; }; \
	mkdir -p bin && install -m 0755 "$$t/typos" bin/typos && echo -e "$(C_OK)typos $(TYPOS_VERSION) installed (checksum verified)"

# =============================================================================
# SETUP
# =============================================================================
.PHONY: bootstrap adopt toolchain setup-env
bootstrap:  ## Set up a fresh box: toolchain, install, configure
	@$(MAKE) toolchain
	@$(MAKE) install
	@$(if $(filter 1,$(CHECK)),$(BIN) configure --check,$(BIN) configure)

INTO    ?= $(HOME)/.claude
ADOPTED  = $(shell [ -e "$(INTO)/.git" ] && echo 1)
# Not spelled $(MAKE), so `make -n adopt` prints the bootstrap line instead of entering a destination not adopted yet.
ADOPT_MAKE = $(MAKE)
adopt:  ## Stage clone: adopt INTO (~/.claude), then bootstrap
	@$(if $(ADOPTED),echo "$(INTO) is already a checkout")
	@$(if $(ADOPTED),,$(MAKE) toolchain compile)
	@$(if $(ADOPTED),,CLAUDE_CONFIG_DIR="$(INTO)" $(BIN) install --setup adopt --from "$(CURDIR)" --into "$(INTO)" $(ADOPT_ARGS))
	@$(if $(or $(ADOPTED),$(findstring --dry-run,$(ADOPT_ARGS))),,$(ADOPT_MAKE) -C "$(INTO)" bootstrap)

toolchain:  ## Install go.mod's Go and pinned tsc to ~/.local if missing
	@if command -v go >/dev/null 2>&1; then echo -e "$(C_OK)go found: $$(go version) ($$(command -v go))"; else \
	os=$$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$$(uname -m); \
	case "$$arch" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; esac; \
	command -v curl >/dev/null 2>&1 || { echo "toolchain: curl not found; install curl and re-run" >&2; exit 1; }; \
	[ ! -e "$(GO_LOCAL_ROOT)" ] || { echo "toolchain: $(GO_LOCAL_ROOT) exists but has no go on PATH; remove it and re-run" >&2; exit 1; }; \
	tgz="go$(GO_VERSION).$$os-$$arch.tar.gz"; url="https://dl.google.com/go/$$tgz"; tmp=$$(mktemp -d) || { echo "toolchain: mktemp failed" >&2; exit 1; }; \
	fail() { echo "toolchain: $$1" >&2; rm -rf "$$tmp"; exit 1; }; $(SHA256) \
	echo "downloading $$url"; \
	curl -fsSL -o "$$tmp/$$tgz" "$$url" || fail "download failed: $$url"; \
	curl -fsSL -o "$$tmp/$$tgz.sha256" "$$url.sha256" || fail "checksum download failed: $$url.sha256"; \
	[ "$$(sha256 "$$tmp/$$tgz")" = "$$(cat "$$tmp/$$tgz.sha256")" ] || fail "sha256 mismatch for $$tgz"; \
	tar -C "$$tmp" -xzf "$$tmp/$$tgz" || fail "extract failed: $$tgz"; \
	mkdir -p "$(dir $(GO_LOCAL_ROOT))" || fail "cannot create $(dir $(GO_LOCAL_ROOT))"; \
	mv "$$tmp/go" "$(GO_LOCAL_ROOT)" || fail "cannot move go to $(GO_LOCAL_ROOT)"; \
	rm -rf "$$tmp"; \
	"$(GO_LOCAL_ROOT)/bin/go" version >/dev/null 2>&1 || { echo "toolchain: $(GO_LOCAL_ROOT)/bin/go does not run" >&2; exit 1; }; \
	echo -e "$(C_OK)installed $$($(GO_LOCAL_ROOT)/bin/go version) -> $(GO_LOCAL_ROOT)"; \
	echo -e "$(C_NOTE)add it to your shell: make setup-env"; fi; \
	if command -v tsc >/dev/null 2>&1; then echo -e "$(C_OK)tsc found: $$(tsc --version) ($$(command -v tsc))"; \
	elif ! command -v npm >/dev/null 2>&1; then echo -e "$(C_NOTE)tsc skipped: npm not found (it ships as an npm package)"; \
	else npm install --global --prefix "$(TS_LOCAL_PREFIX)" --no-fund --no-audit "typescript@$(TS_VERSION)" && \
	echo -e "$(C_OK)installed $$($(TS_LOCAL_PREFIX)/bin/tsc --version) -> $(TS_LOCAL_PREFIX)/bin/tsc"; fi

setup-env:  ## Write the PATH line to the shell rc; DRY_RUN=1 prints it
	@d="$(CURDIR)"; [ "$$d" = "$$HOME/.claude" ] && d='$$HOME/.claude'; \
	line="export PATH=\"$$d/bin:\$$HOME/.local/bin:\$$HOME/.local/go/bin:\$$HOME/go/bin:\$$PATH\""; \
	echo "$$line"; \
	if [ -z "$(DRY_RUN)" ]; then \
		rc="$$HOME/.bashrc"; case "$${SHELL:-}" in */zsh) rc="$$HOME/.zshrc";; esac; \
		if grep -qxF "$$line" "$$rc" 2>/dev/null; then \
			echo -e "$(C_NOTE)already present in $$rc"; \
		else \
			printf '\n# claude-code: %s/bin, ~/.local/bin, ~/.local/go/bin, ~/go/bin on PATH\n%s\n' "$$d" "$$line" >> "$$rc"; \
			echo -e "$(C_OK)appended to $$rc"; \
		fi; \
	fi

# =============================================================================
# QUALITY
# =============================================================================
# One lint per kind (`make lint-<kind>`); WHAT narrows to one language, SKIP=a,b drops kinds.
LINT_WHAT  := go quality shell yaml help docs settings
WHAT       ?= all
SKIP       ?=
C_COMMA    := ,
LINT_KINDS := $(filter-out $(subst $(C_COMMA), ,$(SKIP)),$(or $(if $(filter go,$(WHAT)),go quality),$(if $(filter md,$(WHAT)),docs),$(filter $(WHAT),settings),$(LINT_WHAT)))
FMT_KINDS  := $(filter go yaml docs settings,$(LINT_KINDS))

.PHONY: format lint test
format:  ## Auto-fix go, yaml, md, settings, memory; checks nothing
	@$(CHECK_WHAT)
	$(GO) fmt $(PKGS) >/dev/null
	@if command -v golangci-lint >/dev/null; then golangci-lint fmt $(GO_FMT_DIRS); fi
	@$(if $(FMT_KINDS),$(MAKE) --no-print-directory -k $(addprefix lint-,$(FMT_KINDS)) CHECK= || :)
	@echo -e "$(C_OK)format applied; make lint shows what is left"

lint:  ## Check only, never writes: go, shell, yaml, md, js, memory
	@$(CHECK_WHAT)
	@$(if $(SKIP),echo -e "$(C_NOTE)lint skipping: $(SKIP)")
	@$(MAKE) --no-print-directory -k $(addprefix lint-,$(LINT_KINDS)) CHECK=1
	@echo -e "$(C_OK)lint passed"

lint-%: BIN := $(LINT_BIN)
lint-%: compile mdsmith  # (lint, format) one kind of $(LINT_WHAT); CHECK=1 reports, else fixes
	@case "$*" in \
	go) if [ "$(CHECK)" = 1 ]; then g=gofmt,vet; \
			if command -v golangci-lint >/dev/null; then g=$$g,golangci-lint; else $(NO_GOLANGCI); fi; \
			$(BIN) agents diff --check --all --gates $$g; \
		else gofmt -w $(GO_FMT_DIRS); \
			if command -v golangci-lint >/dev/null; then \
				golangci-lint run --fix --disable gocognit --max-issues-per-linter=0 --max-same-issues=0 ./... || :; \
			else $(NO_GOLANGCI); fi; fi ;; \
	quality) $(BIN) review simplify --fail ;; \
	shell) $(BIN) agents diff --check --all --gates shell ;; \
	yaml) if [ "$(CHECK)" = 1 ]; then $(BIN) agents diff --check --all --gates yaml; \
		elif ! command -v yamlfmt >/dev/null 2>&1; then echo -e "$(C_NOTE)yamlfmt not installed — skipped (make install)"; \
		elif [ -n "$(YAML_FILES)" ]; then yamlfmt $(YAML_FILES); fi ;; \
	help) ;; \
	docs) live="$${CLAUDE_CONFIG_DIR:-$$HOME/.claude}"; \
		if [ "$(CHECK)" = 1 ]; then rc=0; $(BIN) agents diff --check --all --gates docs || rc=1; \
			mdl="state/harness/markdownlint"; [ -d "$$mdl/node_modules" ] || mdl="$$live/$$mdl"; \
			[ ! -f "$$mdl/node_modules/markdownlint-cli2/package.json" ] || \
				grep -qF '"version": "$(MARKDOWNLINT_VERSION)"' "$$mdl/node_modules/markdownlint-cli2/package.json" || \
				echo -e "$(C_NOTE)markdownlint-cli2 in $$mdl is not $(MARKDOWNLINT_VERSION): run make markdownlint"; \
			if [ -x "$$mdl/node_modules/.bin/markdownlint-cli2" ] && command -v node >/dev/null 2>&1; then \
				git ls-files -z --cached --others --exclude-standard '*.md' | xargs -0 "$$mdl/node_modules/.bin/markdownlint-cli2" || rc=1; \
			else echo -e "$(C_NOTE)markdownlint-cli2 skipped: needs node and make markdownlint"; fi; exit $$rc; \
	else rc=0; $(MDSMITH) fix || rc=1; \
		if [ "$$(realpath "$(CURDIR)" 2>/dev/null)" = "$$(realpath "$$live" 2>/dev/null)" ]; then \
			$(BIN) memory fmt || rc=1; \
		else echo -e "$(C_NOTE)memory fmt skipped: $(CURDIR) is not the live config dir ($$live) — a worktree must not write the shared memory store"; fi; \
		exit $$rc; fi ;; \
	settings) $(BIN) configure fmt ;; \
	*) printf "Unknown lint '%s' (use %s)\n" "$*" "$(subst $(C_SP),|,$(LINT_WHAT))"; exit 1 ;; \
	esac
	@$(if $(filter help,$*),$(MAKE) --no-print-directory help-check)

COVER_ROWS = | awk '/coverage:/ {print $$2, $$(NF-2)}' | sort -t' ' -k2 -n
test:  ## Go tests, -short by default; FULL=1 runs all, COVER=1 lists coverage
	@$(GO) test $(if $(or $(FULL),$(COVER)),,-short )$(if $(COVER),-cover )-timeout $(GO_TEST_TIMEOUT) $(TEST_PKGS) $(if $(COVER),$(COVER_ROWS))

# =============================================================================
# AUDIT
# =============================================================================
# Stages run in parallel on LINT_BIN, one log each, each in its own process group (set -m) so a cancel reaches
# grandchildren; agents sync, the one writer, runs alone after. AUDIT_INNER=1 skips the sub-targets' own compile.
AUDIT_LINT   ?= $(MAKE) lint SKIP=docs
AUDIT_TEST   ?= if [ "$(SKIP_TESTS)" = 1 ]; then echo -e "$(C_NOTE)tests skipped (SKIP_TESTS=1)"; else $(BIN) test ramdisk -- $(MAKE) test FULL=1; fi
# One check per kind (`make audit-<kind>`); sync writes, so audit runs it alone after the parallel ones.
AUDIT_WHAT   := git vuln prompts sync
AUDIT_CHECKS ?= $(MAKE) -k $(addprefix audit-,$(filter-out sync,$(AUDIT_WHAT))) BIN=$(BIN) AUDIT_INNER=1
AUDIT_PRE    := $(if $(AUDIT_INNER),,compile)
# One `go test` at a time: it already spans the cores; seven at once flaked wall-clock tests (31s vs 33s sharded).
TEST_PKGS    ?= $(PKGS)
# audit-git range: AUDIT_GIT_RANGE, else @{u}..HEAD, else the last AUDIT_GIT_FALLBACK commits.
AUDIT_GIT_FALLBACK ?= 20

.PHONY: audit verify
audit: BIN := $(LINT_BIN)
audit: compile  ## Local gate: lint, git secrets, test, build, vuln, docs
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

verify: audit  ## audit + doctor + proxy status
	@$(BIN) doctor
	@$(BIN) gateway proxy --status
	@echo -e "$(C_OK)verify passed (audit + doctor + proxy status)"

audit-%: BIN := $(LINT_BIN)
audit-%: $(AUDIT_PRE)  # (audit) one check of $(AUDIT_WHAT)
	@case "$*" in \
	git) if [ -n "$(AUDIT_GIT_RANGE)" ]; then exec $(BIN) doctor --git --range '$(AUDIT_GIT_RANGE)'; fi; \
		if ! n=$$(git rev-list --count '@{u}..HEAD' 2>/dev/null); then exec $(BIN) doctor --git --since $(AUDIT_GIT_FALLBACK); fi; \
		if [ "$$n" = 0 ]; then echo -e "$(C_NOTE)doctor --git skipped (no unpushed commits)"; exit 0; fi; \
		$(BIN) doctor --git --range '@{u}..HEAD' ;; \
	vuln) if ! command -v govulncheck >/dev/null 2>&1; then echo -e "$(C_NOTE)govulncheck not installed — skipped (make install)"; \
		elif ! curl -fsS -m 5 -o /dev/null https://vuln.go.dev/index/db.json 2>/dev/null; then \
			echo -e "$(C_NOTE)govulncheck skipped (vuln.go.dev unreachable)"; \
		else govulncheck $(PKGS); fi ;; \
	prompts) $(BIN) doctor --prompts ;; \
	sync) out="$$(CLAUDE_CONFIG_DIR=$(CURDIR) $(BIN) agents sync --dry-run 2>&1)" || { echo "$$out"; \
		echo -e "$(C_NOTE)sync drift: running agents sync"; CLAUDE_CONFIG_DIR=$(CURDIR) $(BIN) agents sync || exit 1; } ;; \
	*) printf "Unknown audit '%s' (use %s)\n" "$*" "$(subst $(C_SP),|,$(AUDIT_WHAT))"; exit 1 ;; \
	esac

# =============================================================================
# CONTAINER
# =============================================================================
TARGET   ?= nested-podman
IMAGE    ?= claude-code-harness:$(TARGET)
IMAGE_SH  = TARGET=$(TARGET) IMAGE=$(IMAGE) container/image.sh

.PHONY: image image-audit image-shell
image:  ## Ubuntu image (TARGET=base|nested-podman)
	$(IMAGE_SH) build

image-audit: image  ## Run make audit inside a fresh Ubuntu container
	$(IMAGE_SH) run make audit

image-shell:  ## Open a shell in the built image (no rebuild)
	$(IMAGE_SH) run bash

# =============================================================================
# DIAGNOSTICS
# =============================================================================
.PHONY: statusline
statusline: BIN := $(LINT_BIN)
statusline: compile  ## Render a sample statusline (COLS=…)
	@echo '{"model":{"id":"claude-sonnet-5","display_name":"Sonnet 5"},"workspace":{"current_dir":"'$(CURDIR)'"},"transcript_path":"/tmp/x","context_window":{"context_window_size":1000000,"used_percentage":42}}' \
		| COLUMNS=$${COLS:-120} $(BIN) configure theme statusline
