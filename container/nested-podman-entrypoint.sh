#!/usr/bin/env bash
# Configure rootless podman for the current user, then exec the command (adapted from ai-helpers' entrypoint).
set -euo pipefail

conf="${HOME}/.config/containers"
mkdir -p "${conf}"

if [ ! -f "${conf}/registries.conf" ]; then
  cat > "${conf}/registries.conf" <<'EOF'
unqualified-search-registries = ["docker.io"]
short-name-mode = "permissive"
EOF
fi

# No systemd in the container: cgroupfs and file events, host netns for nested runs.
if [ ! -f "${conf}/containers.conf" ]; then
  cat > "${conf}/containers.conf" <<'EOF'
[containers]
netns = "host"
userns = "host"
ipcns = "host"
utsns = "host"
cgroupns = "host"
cgroups = "disabled"
log_driver = "k8s-file"
[engine]
cgroup_manager = "cgroupfs"
events_logger = "file"
runtime = "crun"
EOF
fi

if [ ! -f "${conf}/storage.conf" ]; then
  if [ -c /dev/fuse ] && [ -x /usr/bin/fuse-overlayfs ]; then
    cat > "${conf}/storage.conf" <<'EOF'
[storage]
driver = "overlay"
graphroot = "/tmp/graphroot"
[storage.options.overlay]
mount_program = "/usr/bin/fuse-overlayfs"
EOF
  else
    printf '[storage]\ndriver = "vfs"\ngraphroot = "/tmp/graphroot"\n' > "${conf}/storage.conf"
  fi
fi

# Sub-IDs must fit inside the uid range the outer runtime mapped in (often only 0-65535 when it is rootless).
start_default=$(( $(id -u) + 1 ))
count_default=$(( 65536 - start_default ))
if (( count_default <= 0 )); then
  start_default=100000
  count_default=65536
fi
user_name="$(id -un)"
echo "${user_name}:${SUBID_START:-$start_default}:${SUBID_COUNT:-$count_default}" > /etc/subuid
echo "${user_name}:${SUBID_START:-$start_default}:${SUBID_COUNT:-$count_default}" > /etc/subgid

exec "$@"
