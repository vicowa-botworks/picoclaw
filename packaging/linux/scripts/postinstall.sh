#!/bin/sh
# Enable the gateway at boot after the package lays down its unit file.
#
# Only picoclaw.service is enabled. picoclaw-launcher.service ships alongside it
# but stays disabled: it is a configuration console, not a 24/7 daemon, and on a
# 1GB Pi 4 it is worth the RAM you get back.
#
# A masked unit is left strictly alone. `systemctl mask` is how an operator says
# "never start this", and a package upgrade must not overrule that.
set -eu

enable_unit() {
    unit=$1

    # `systemctl mask` symlinks /etc/systemd/system/<unit> to /dev/null, and
    # /dev/null exists — so `[ ! -e "$path" ]` is the wrong probe. It fires for a
    # dangling symlink and misses every real mask, which would let a masked unit
    # through to `systemctl enable` (fails) and then to the deb-systemd-helper
    # fallback below, which never asks systemd and would write "enabled"
    # bookkeeping over an operator's explicit "never start this".
    # Read the link instead. systemd may not be running at package-install time
    # (chroot, container build), so this stays a filesystem test.
    if [ "$(readlink "/etc/systemd/system/$unit" 2>/dev/null)" = /dev/null ]; then
        echo "picoclaw: $unit is masked, not enabling it."
        return 0
    fi

    # `systemctl enable` is the primary mechanism, not deb-systemd-helper.
    #
    # Verified on Debian trixie: `deb-systemd-helper enable` alone records
    # bookkeeping under /var/lib/systemd/deb-systemd-helper-enabled/ but does
    # NOT create the .wants symlink — it declines for a unit it has never seen
    # enabled before. A package that relies on it alone installs "disabled",
    # which for this bundle means silently never starting at boot.
    # `systemctl enable` creates the symlink whether or not systemd is PID 1,
    # so it is correct in a chroot and on a running system alike.
    enabled=0
    if command -v systemctl >/dev/null 2>&1; then
        if systemctl enable "$unit" >/dev/null 2>&1; then
            enabled=1
        fi
    fi
    if [ "$enabled" -eq 0 ] && command -v deb-systemd-helper >/dev/null 2>&1; then
        deb-systemd-helper enable -q --root / "$unit" >/dev/null 2>&1 || true
    fi

    # Keep dpkg's own record current so a later upgrade or removal can tell that
    # the package enabled this unit.
    if command -v deb-systemd-helper >/dev/null 2>&1; then
        deb-systemd-helper update-state -q --root / "$unit" >/dev/null 2>&1 || true
    fi

    if [ "$enabled" -eq 0 ]; then
        echo "picoclaw: could not enable $unit; run: systemctl enable $unit"
    fi

    # systemd may not be PID 1 here (chroot, docker build). Ignore, not fail.
    systemctl daemon-reload >/dev/null 2>&1 || true

    # Pick up the new binary on upgrade, but only if it was already running.
    #
    # `try-restart` is the whole point: it restarts an active unit and does
    # nothing to an inactive one. An unconditional `restart` would try to start
    # the gateway on a fresh install, where config.json does not exist yet, and
    # would also resurrect a daemon the operator had stopped on purpose.
    systemctl try-restart "$unit" >/dev/null 2>&1 || true
    return 0
}

enable_unit picoclaw.service

# Seed the secrets file from the template.
#
# The package ships /etc/picoclaw/env.example and nothing else, so without this
# the operator follows the message below straight into `sudoedit /etc/picoclaw/env`
# and CREATES the file that will hold provider API keys with whatever owner and
# mode their editor and umask happen to hand out — not the 0640 root:picoclaw the
# design calls for. install.sh already does this for the tar.gz path; the two
# install paths must not diverge, and README documents this one.
if [ -f /etc/picoclaw/env.example ] && [ ! -f /etc/picoclaw/env ] &&
    getent group picoclaw >/dev/null 2>&1; then
    install -m 0640 -o root -g picoclaw /etc/picoclaw/env.example /etc/picoclaw/env
fi

echo
echo "picoclaw installed and enabled at boot."
echo "It stays stopped until /var/lib/picoclaw/config.json exists:"
echo "    sudo -u picoclaw PICOCLAW_HOME=/var/lib/picoclaw picoclaw onboard"
echo "    sudoedit /etc/picoclaw/env      # API keys go here, not in config.json"
echo "    sudo systemctl start picoclaw"
echo
