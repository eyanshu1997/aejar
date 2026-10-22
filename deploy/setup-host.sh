#!/usr/bin/env bash
#
# setup-host.sh: one-time host setup for aejar on its server. Run it as the
# owner with sudo:
#
#   sudo bash deploy/setup-host.sh [--disable-kubelet] [--data-disk /mnt/disk]
#
# --data-disk puts aejar's disk-heavy state on another (ext4) disk: the
# rootless Docker storage (images, container layers) under
# <disk>/aejar/docker, and the data dir under <disk>/aejar/data. Set
# AEJAR_DATA=<disk>/aejar/data in aejar.env afterwards.
#
# It creates a dedicated `aejar` user (no sudo, not in the docker group) with
# its own rootless Docker daemon, so nothing aejar runs can reach root or the
# existing containers. Safe to re-run: every step checks before it changes.

set -euo pipefail

AEJAR_USER=aejar
DISABLE_KUBELET=false
DATA_DISK=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --disable-kubelet) DISABLE_KUBELET=true ;;
        --data-disk) DATA_DISK="${2:?--data-disk needs a path}"; shift ;;
        *) echo "unknown option $1" >&2; exit 1 ;;
    esac
    shift
done

if [[ $EUID -ne 0 ]]; then
    echo "run with sudo" >&2
    exit 1
fi
OWNER="${SUDO_USER:?run via sudo from your own account}"

step() { printf '\n== %s\n' "$*"; }

step "packages for rootless docker"
apt-get install -y uidmap dbus-user-session slirp4netns docker-ce-rootless-extras >/dev/null

step "user $AEJAR_USER"
if ! id "$AEJAR_USER" >/dev/null 2>&1; then
    useradd --create-home --shell /bin/bash "$AEJAR_USER"
fi
# Must not be root-equivalent.
for g in docker sudo; do
    if id -nG "$AEJAR_USER" | tr ' ' '\n' | grep -qx "$g"; then
        gpasswd -d "$AEJAR_USER" "$g"
    fi
done
AEJAR_UID=$(id -u "$AEJAR_USER")
AEJAR_HOME=$(getent passwd "$AEJAR_USER" | cut -d: -f6)

step "subordinate uid/gid ranges"
grep -q "^$AEJAR_USER:" /etc/subuid || usermod --add-subuids 300000-365535 "$AEJAR_USER"
grep -q "^$AEJAR_USER:" /etc/subgid || usermod --add-subgids 300000-365535 "$AEJAR_USER"

step "ssh access for $OWNER as $AEJAR_USER (deploys and attaching to sessions)"
install -d -m 700 -o "$AEJAR_USER" -g "$AEJAR_USER" "$AEJAR_HOME/.ssh"
OWNER_HOME=$(getent passwd "$OWNER" | cut -d: -f6)
if [[ -f "$OWNER_HOME/.ssh/authorized_keys" ]]; then
    install -m 600 -o "$AEJAR_USER" -g "$AEJAR_USER" "$OWNER_HOME/.ssh/authorized_keys" "$AEJAR_HOME/.ssh/authorized_keys"
fi

step "AppArmor: allow rootlesskit to create user namespaces"
if [[ "$(sysctl -n kernel.apparmor_restrict_unprivileged_userns 2>/dev/null || echo 0)" == "1" ]]; then
    RK=$(command -v rootlesskit || echo /usr/bin/rootlesskit)
    PROFILE="/etc/apparmor.d/$(echo "${RK#/}" | tr / .)"
    if [[ ! -f "$PROFILE" ]]; then
        cat > "$PROFILE" <<EOF
abi <abi/4.0>,
include <tunables/global>

"$RK" flags=(unconfined) {
  userns,

  include if exists <local/$(basename "$PROFILE")>
}
EOF
        systemctl restart apparmor.service
    fi
fi

step "linger (keeps $AEJAR_USER's services running without a login)"
loginctl enable-linger "$AEJAR_USER"
for _ in $(seq 1 20); do
    [[ -S "/run/user/$AEJAR_UID/bus" ]] && break
    sleep 0.5
done

as_aejar() {
    sudo -iu "$AEJAR_USER" env XDG_RUNTIME_DIR="/run/user/$AEJAR_UID" \
        DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$AEJAR_UID/bus" "$@"
}

if [[ -n "$DATA_DISK" ]]; then
    step "aejar state on $DATA_DISK"
    fstype=$(findmnt -no FSTYPE --target "$DATA_DISK" | tail -1)
    if [[ "$fstype" != "ext4" && "$fstype" != "xfs" && "$fstype" != "btrfs" ]]; then
        echo "$DATA_DISK is $fstype; docker storage needs ext4, xfs or btrfs" >&2
        exit 1
    fi
    install -d -m 700 -o "$AEJAR_USER" -g "$AEJAR_USER" "$DATA_DISK/aejar" "$DATA_DISK/aejar/docker" "$DATA_DISK/aejar/data"
    install -d -m 700 -o "$AEJAR_USER" -g "$AEJAR_USER" "$AEJAR_HOME/.config" "$AEJAR_HOME/.config/docker"
    printf '{ "data-root": "%s" }\n' "$DATA_DISK/aejar/docker" > "$AEJAR_HOME/.config/docker/daemon.json"
    chown "$AEJAR_USER:$AEJAR_USER" "$AEJAR_HOME/.config/docker/daemon.json"
fi

step "rootless docker daemon for $AEJAR_USER"
if ! as_aejar systemctl --user is-active --quiet docker; then
    as_aejar dockerd-rootless-setuptool.sh install
fi
as_aejar systemctl --user enable --now docker
# Pick up a changed data-root.
if [[ -n "$DATA_DISK" ]]; then as_aejar systemctl --user restart docker; fi
grep -q DOCKER_HOST "$AEJAR_HOME/.bashrc" ||
    echo "export DOCKER_HOST=unix:///run/user/$AEJAR_UID/docker.sock" >> "$AEJAR_HOME/.bashrc"

step "data directories"
install -d -m 700 -o "$AEJAR_USER" -g "$AEJAR_USER" \
    "$AEJAR_HOME/aejar-data" "$AEJAR_HOME/aejar-data/secrets" "$AEJAR_HOME/aejar-data/sessions" \
    "$AEJAR_HOME/aejar-data/coherence" "$AEJAR_HOME/aejar-data/db"
if [[ -n "$DATA_DISK" ]]; then
    install -d -m 700 -o "$AEJAR_USER" -g "$AEJAR_USER" \
        "$DATA_DISK/aejar/data/secrets" "$DATA_DISK/aejar/data/sessions" \
        "$DATA_DISK/aejar/data/coherence" "$DATA_DISK/aejar/data/db" "$DATA_DISK/aejar/data/config"
fi

if $DISABLE_KUBELET; then
    step "disable leftover kubelet (crash-looping, no cluster config)"
    systemctl disable --now kubelet 2>/dev/null || true
fi

step "check"
as_aejar env DOCKER_HOST="unix:///run/user/$AEJAR_UID/docker.sock" docker info --format \
    'rootless={{.SecurityOptions}} server={{.ServerVersion}} data-root={{.DockerRootDir}}'
id "$AEJAR_USER"
echo
echo "done. aejar's docker socket: /run/user/$AEJAR_UID/docker.sock"
