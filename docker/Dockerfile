# Ubuntu images that install and audit this harness the same way every time.
# Ported from ai-helpers images/Dockerfile (target base) and Dockerfile.nested-podman (target nested-podman).
# Build with docker/image.sh build (context = git's view of the checkout: no vault, memory or sessions).

# The "latest" channel binary, kept beside stable as claude-latest to compare versions.
FROM ubuntu:24.04 AS claude-latest
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL https://claude.ai/install.sh | bash -s latest \
    && cp -L /root/.local/bin/claude /claude-latest

FROM ubuntu:24.04 AS base
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git make gcc libc6-dev jq unzip xz-utils less \
      nodejs shellcheck gh \
      python3 python3-pip python3-venv python3-pytest python3-requests python3-yaml \
    && rm -rf /var/lib/apt/lists/*

COPY --from=claude-latest /claude-latest /usr/local/bin/claude-latest

# The complete Codex CLI package (code-mode host and sandbox tools), checksum-pinned per arch.
ARG CODEX_VERSION=0.149.0
ARG CODEX_SHA256_AMD64=1c08ba262820b78d49ea7a93f326b6b430b72e5fe46830e433edef12e5123244
ARG CODEX_SHA256_ARM64=dac03e4db966e94f7083b9eb9f95d8b40da9d568fd2fdbb6885b0ea7d5b0d97f
RUN case "$(uname -m)" in \
      x86_64) CODEX_ARCH=x86_64; CODEX_SHA256="${CODEX_SHA256_AMD64}" ;; \
      aarch64) CODEX_ARCH=aarch64; CODEX_SHA256="${CODEX_SHA256_ARM64}" ;; \
      *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;; \
    esac \
    && curl -fsSL -o /tmp/codex.tar.gz \
      "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-package-${CODEX_ARCH}-unknown-linux-musl.tar.gz" \
    && echo "${CODEX_SHA256}  /tmp/codex.tar.gz" | sha256sum -c - \
    && mkdir -p /opt/codex \
    && tar -xzf /tmp/codex.tar.gz -C /opt/codex \
    && rm /tmp/codex.tar.gz
COPY docker/codex-wrapper.sh /usr/local/bin/codex
RUN chmod 0755 /usr/local/bin/codex

# Ubuntu ships a uid-1000 `ubuntu` user; replace it so the harness user owns uid 1000.
RUN userdel -r ubuntu 2>/dev/null || true; useradd -m -u 1000 -s /bin/bash claude

USER claude
ENV HOME=/home/claude \
    CLAUDE_CONFIG_DIR=/home/claude/.claude \
    CODEX_HOME=/home/claude/.codex \
    PATH=/home/claude/.claude/bin:/home/claude/.local/bin:/home/claude/.local/go/bin:/home/claude/go/bin:/usr/local/bin:/opt/codex/bin:/opt/codex/codex-path:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin

# Claude Code stable from the official native installer; CLAUDE_CHANNEL=stable|latest|<version>.
ARG CLAUDE_CHANNEL=stable
RUN curl -fsSL https://claude.ai/install.sh | bash -s "${CLAUDE_CHANNEL}"

# The checkout is ~/.claude, as on a real box; a one-commit repo keeps git-based checks working.
COPY --chown=claude:claude . /home/claude/.claude
WORKDIR /home/claude/.claude
RUN git init -q -b main \
    && git add -A \
    && git -c user.name=image -c user.email=image@localhost commit -qm "image snapshot"

# go.mod's Go (sha256-checked) into ~/.local/go, then the harness and its pinned gate tools.
# GO_JOBS caps parallel compiles: anthropic-sdk-go alone OOMs a 2 GiB podman machine at the default.
ARG GO_JOBS=2
RUN make toolchain &&GOFLAGS="-p=${GO_JOBS}" make install DEV_TOOLS=1

ENTRYPOINT ["claude"]
CMD ["--help"]

# Same image plus rootless podman, so tests can run containers inside it.
FROM base AS nested-podman
USER root
RUN apt-get update && apt-get install -y --no-install-recommends \
      podman uidmap fuse-overlayfs slirp4netns passt catatonit libcap2-bin \
    && rm -rf /var/lib/apt/lists/* \
    && chmod 0755 /usr/bin/newuidmap /usr/bin/newgidmap \
    && setcap cap_setuid+ep /usr/bin/newuidmap \
    && setcap cap_setgid+ep /usr/bin/newgidmap \
    && touch /etc/subuid /etc/subgid \
    && chown claude /etc/subuid /etc/subgid
COPY docker/nested-podman-entrypoint.sh /usr/local/bin/nested-podman-entrypoint.sh
RUN chmod 0755 /usr/local/bin/nested-podman-entrypoint.sh
ENV BUILDAH_ISOLATION=chroot
USER claude

# catatonit reaps zombies; the entrypoint configures rootless podman, then execs the command.
ENTRYPOINT ["/usr/bin/catatonit", "--", "/usr/local/bin/nested-podman-entrypoint.sh"]
CMD ["bash"]
