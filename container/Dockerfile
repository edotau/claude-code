FROM ubuntu:24.04 AS base
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git make gcc libc6-dev jq unzip xz-utils less \
      nodejs npm shellcheck gh \
      python3 python3-pip python3-venv python3-pytest python3-requests python3-yaml \
    && rm -rf /var/lib/apt/lists/*

# Ubuntu ships a uid-1000 `ubuntu` user; replace it so the harness user owns uid 1000.
RUN userdel -r ubuntu 2>/dev/null || true; useradd -m -u 1000 -s /bin/bash claude

USER claude
ENV HOME=/home/claude \
    CLAUDE_CONFIG_DIR=/home/claude/.claude \
    CODEX_HOME=/home/claude/.codex \
    PATH=/home/claude/.claude/bin:/home/claude/.local/bin:/home/claude/.local/go/bin:/home/claude/go/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin

# The checkout is ~/.claude, as on a real box; a one-commit repo keeps git-based checks working.
COPY --chown=claude:claude . /home/claude/.claude
WORKDIR /home/claude/.claude
RUN git init -q -b main \
    && git add -A \
    && git -c user.name=image -c user.email=image@localhost commit -qm "image snapshot"

# `make bootstrap` minus its configure step (no credentials at build time): go.mod's Go into ~/.local/go, then the
# harness, its gate tools and every missing agent CLI via `claude-code install --tools --missing`.
# GO_JOBS caps parallel compiles: anthropic-sdk-go alone OOMs a 2 GiB podman machine at the default.
ARG GO_JOBS=2
RUN make toolchain && GOFLAGS="-p=${GO_JOBS}" make install

ENTRYPOINT ["claude-code"]
CMD ["-h"]

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
COPY container/nested-podman-entrypoint.sh /usr/local/bin/nested-podman-entrypoint.sh
RUN chmod 0755 /usr/local/bin/nested-podman-entrypoint.sh
ENV BUILDAH_ISOLATION=chroot
USER claude

# catatonit reaps zombies; the entrypoint configures rootless podman, then execs the image's own claude-code.
ENTRYPOINT ["/usr/bin/catatonit", "--", "/usr/local/bin/nested-podman-entrypoint.sh", "claude-code"]
CMD ["-h"]
