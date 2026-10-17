#!/bin/sh
# Stop and disable the units when the package is REMOVED.
#
# Deliberately does nothing on upgrade: stopping there would leave the daemon
# down after the new package is unpacked. Restart-on-upgrade is handled by the
# mark-restart recorded in postinstall.sh.
set -eu

case "${1:-}" in
    upgrade | failed-upgrade | abort-install | abort-upgrade | disappear)
        exit 0
        ;;
esac

for unit in picoclaw.service picoclaw-launcher.service; do
    systemctl --system stop "$unit" >/dev/null 2>&1 || true
    systemctl --system disable "$unit" >/dev/null 2>&1 || true
done

exit 0
