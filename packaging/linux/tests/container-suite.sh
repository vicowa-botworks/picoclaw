#!/usr/bin/env bash
# End-to-end packaging test: real RPM install / upgrade / erase against a
# BOOTED systemd, in a container. Manual or pre-release — needs docker and nfpm.
#
#     ./packaging/linux/tests/container-suite.sh
#
# This covers what the stubbed contract test cannot: the arguments RPM actually
# passes to each scriptlet, the ORDER it passes them in (the old package's
# %preun runs AFTER the new package's %post), and whether the unit is still
# enabled and running once the transaction finishes.
#
# Runs natively on x86_64. Do NOT run this under qemu-aarch64: every unit here
# uses namespace directives and they die with status=226/NAMESPACE under
# emulation. Sandbox behaviour is arch-independent, so x86_64 is the honest host.
set -uo pipefail

REPO=$(cd "$(dirname "$0")/../../.." && pwd)
PKG="$REPO/packaging/linux"
W=$(mktemp -d)
trap 'docker rm -f pc-pkg-test >/dev/null 2>&1; rm -rf "$W"' EXIT

for tool in docker nfpm; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "container-suite: $tool is required" >&2; exit 2; }
done

PASS=0
FAIL=0
chk() {
    if [ "$2" = "$3" ]; then echo "PASS | $1 | $3"; PASS=$((PASS + 1));
    else echo "FAIL | $1 | expected [$2] got [$3]"; FAIL=$((FAIL + 1)); fi
}

echo "==> building a systemd-booting test image"
cat > "$W/Dockerfile" <<'EOF'
FROM debian:trixie-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get -y --no-install-recommends install \
      systemd systemd-sysv dbus rpm ca-certificates \
 && rm -rf /var/lib/apt/lists/*
STOPSIGNAL SIGRTMIN+3
CMD ["/lib/systemd/systemd"]
EOF
docker build -q -t picoclaw-systemd-test "$W" >/dev/null

echo "==> building three RPM versions from the scripts in this tree"
printf '#!/bin/sh\nexec sleep 3600\n' > "$W/picoclaw"
chmod 0755 "$W/picoclaw"
for v in 1.0.0 1.0.1 1.0.2; do
    cat > "$W/nfpm-$v.yaml" <<EOF
name: picoclaw
arch: amd64
version: "$v"
release: "1"
description: packaging scriptlet test
maintainer: picoclaw
license: MIT
vendor: picoclaw
contents:
  - src: $W/picoclaw
    dst: /usr/bin/picoclaw
    file_info:
      mode: 0755
  - src: $PKG/systemd/picoclaw.service
    dst: /lib/systemd/system/picoclaw.service
    file_info:
      mode: 0644
  - src: $PKG/systemd/picoclaw-launcher.service
    dst: /lib/systemd/system/picoclaw-launcher.service
    file_info:
      mode: 0644
  - src: $PKG/env.example
    dst: /etc/picoclaw/env.example
    file_info:
      mode: 0644
scripts:
  preinstall: $PKG/scripts/preinstall.sh
  postinstall: $PKG/scripts/postinstall.sh
  preremove: $PKG/scripts/preremove.sh
  postremove: $PKG/scripts/postremove.sh
EOF
    nfpm package -f "$W/nfpm-$v.yaml" -p rpm -t "$W/$v.rpm" >/dev/null
done

cat > "$W/scenario.sh" <<'EOS'
#!/bin/bash
pass=0; fail=0
chk() {
    if [ "$2" = "$3" ]; then echo "PASS | $1 | $3"; pass=$((pass+1));
    else echo "FAIL | $1 | expected [$2] got [$3]"; fail=$((fail+1)); fi
}

echo "--- install 1.0.0 ---"
rpm -ivh /pkgs/1.0.0.rpm > /tmp/inst.log 2>&1
chk "install: gateway enabled at boot" enabled "$(systemctl is-enabled picoclaw.service 2>&1)"
chk "install: secrets file seeded from template" yes "$([ -f /etc/picoclaw/env ] && echo yes || echo no)"
chk "install: secrets file mode" 640 "$(stat -c %a /etc/picoclaw/env 2>/dev/null)"
chk "install: secrets file owner" root:picoclaw "$(stat -c %U:%G /etc/picoclaw/env 2>/dev/null)"
chk "install: launcher left disabled" disabled "$(systemctl is-enabled picoclaw-launcher.service 2>&1)"

echo "--- operator state that must survive an upgrade ---"
echo '{}' > /var/lib/picoclaw/config.json
chown picoclaw:picoclaw /var/lib/picoclaw/config.json
echo 'OPENROUTER_API_KEY=keepme' >> /etc/picoclaw/env
chmod 0700 /var/lib/picoclaw
systemctl start picoclaw.service 2>/dev/null; sleep 2
chk "pre-upgrade: gateway active" active "$(systemctl is-active picoclaw.service 2>&1)"

echo "--- upgrade to 1.0.1 (rpm -Uvh) ---"
rpm -Uvh /pkgs/1.0.1.rpm > /tmp/up.log 2>&1
chk "upgrade: still enabled at boot" enabled "$(systemctl is-enabled picoclaw.service 2>&1)"
chk "upgrade: still active" active "$(systemctl is-active picoclaw.service 2>&1)"
chk "upgrade: secrets file not clobbered" yes "$(grep -q keepme /etc/picoclaw/env && echo yes || echo no)"
chk "upgrade: operator's chmod on state dir kept" 700 "$(stat -c %a /var/lib/picoclaw 2>/dev/null)"

echo "--- second upgrade, with the operator's mask in place ---"
systemctl mask picoclaw.service >/dev/null 2>&1
chk "operator masked the unit" masked "$(systemctl is-enabled picoclaw.service 2>&1)"
rpm -Uvh /pkgs/1.0.2.rpm > /tmp/up2.log 2>&1
chk "masked upgrade: operator's 'never' still wins" masked "$(systemctl is-enabled picoclaw.service 2>&1)"
chk "masked upgrade: reported the mask" yes "$(grep -q 'is masked, not enabling it' /tmp/up2.log && echo yes || echo no)"
chk "masked upgrade: no bogus enable-failure warning" yes "$(grep -q 'could not enable' /tmp/up2.log && echo no || echo yes)"

echo "--- erase ---"
systemctl unmask picoclaw.service >/dev/null 2>&1
rpm -e picoclaw > /tmp/rm.log 2>&1
chk "erase: gateway stopped" inactive "$(systemctl is-active picoclaw.service 2>&1)"
chk "erase: unit file removed" yes "$([ ! -e /lib/systemd/system/picoclaw.service ] && echo yes || echo no)"
chk "erase: boot symlink gone" yes "$([ ! -e /etc/systemd/system/multi-user.target.wants/picoclaw.service ] && echo yes || echo no)"
chk "erase: user data kept" yes "$([ -d /var/lib/picoclaw ] && echo yes || echo no)"

echo "SCENARIO $pass passed, $fail failed"
[ "$fail" -eq 0 ]
EOS

echo "==> running the scenarios in a booted systemd container"
docker rm -f pc-pkg-test >/dev/null 2>&1
# --cgroupns=host is required: without it systemd exits 255 with no output.
docker run -d --privileged --cgroupns=host \
    --tmpfs /run --tmpfs /run/lock --tmpfs /tmp --tmpfs /dev/shm \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw -v "$W:/pkgs:ro" -v "$W:/t:ro" \
    --name pc-pkg-test picoclaw-systemd-test >/dev/null

for _ in $(seq 1 40); do
    state=$(docker exec pc-pkg-test systemctl is-system-running 2>&1)
    case "$state" in running | degraded) break ;; esac
    sleep 1
done
echo "systemd reached: $state"

if docker exec pc-pkg-test bash /t/scenario.sh; then ok=1; else ok=0; fi
docker rm -f pc-pkg-test >/dev/null 2>&1

echo
if [ "$ok" = 1 ]; then echo "container-suite: PASS"; exit 0; fi
echo "container-suite: FAIL"
exit 1
