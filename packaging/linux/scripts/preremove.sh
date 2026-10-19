#!/bin/sh
# Stop and disable the units when the package is REMOVED.
#
# Deliberately does nothing on upgrade: stopping there would leave the daemon
# down after the new package is unpacked. Restart-on-upgrade is handled by the
# mark-restart recorded in postinstall.sh.
set -eu

# Two argument vocabularies have to be recognised here.
#
# dpkg passes a word: `prerm upgrade <version>`, `prerm remove`, and so on.
# RPM passes a NUMBER — the count of instances of the package that will remain
# after this scriptlet: `1` on an upgrade, `0` on an erase. RPM also runs the
# OLD package's %preun AFTER the NEW package's %post, so an unrecognised `1`
# falls through and undoes the upgrade the new package just completed:
#
#     new %post   -> postinstall try-restart   (gateway running)
#     old %preun  -> stop + disable            (stopped, .wants symlink gone)
#
# i.e. every `rpm -Uvh` would end with the gateway stopped and disabled, never
# returning at boot. dpkg never passes a bare number, so accepting one here is
# safe for both.
case "${1:-}" in
    upgrade | failed-upgrade | abort-install | abort-upgrade | disappear)
        exit 0
        ;;
    [1-9]*)
        # RPM: an upgrade, or a multi-instance install. Only a bare `0` is an erase.
        exit 0
        ;;
esac

for unit in picoclaw.service picoclaw-launcher.service; do
    systemctl --system stop "$unit" >/dev/null 2>&1 || true
    systemctl --system disable "$unit" >/dev/null 2>&1 || true
done

exit 0
