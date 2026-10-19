#!/bin/sh
# Clean up after removal.
#
# /var/lib/picoclaw (config, workspace, skills, session memory) and
# /etc/picoclaw (API keys) are left in place on purpose, and the picoclaw
# account is left in place too — other software may have come to own files
# owned by that uid, and deleting a uid is not reversible.
#
# To finish the job by hand:
#     sudo rm -rf /var/lib/picoclaw /etc/picoclaw
#     sudo deluser --remove-home picoclaw    # or: sudo userdel -r picoclaw
set -eu

systemctl daemon-reload >/dev/null 2>&1 || true
# Named units only. A bare `systemctl reset-failed` resets the start-limit
# counter of EVERY unit on the host, which is other services' state to manage.
# The names may already be unloaded by now — the package manager removes the
# unit files before this runs — so the failure is expected and ignored.
systemctl reset-failed picoclaw.service picoclaw-launcher.service >/dev/null 2>&1 || true

echo "picoclaw removed. Your config, workspace and API keys were kept in"
echo "/var/lib/picoclaw and /etc/picoclaw."

exit 0
