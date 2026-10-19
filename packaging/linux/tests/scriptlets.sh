#!/bin/sh
# Scriptlet contract tests. No root, no systemd, no RPM — a stub `systemctl`
# records what each scriptlet would have done, for each argument it can receive.
#
#     sh packaging/linux/tests/scriptlets.sh
#
# The case this exists for: RPM passes %preun a NUMBER (1 = upgrade, 0 = erase)
# while dpkg passes a word. An unrecognised `1` falls through to stop+disable,
# and because RPM runs the OLD package's %preun after the NEW package's %post,
# every `rpm -Uvh` would end with the gateway stopped and disabled.
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")/.." && pwd)
PREREMOVE="$SCRIPT_DIR/scripts/preremove.sh"
STUB=$(mktemp -d)
trap 'rm -rf "$STUB"' EXIT

printf '#!/bin/sh\necho "$*" >> "$CALLS"\n' > "$STUB/systemctl"
chmod +x "$STUB/systemctl"

pass=0
fail=0

both_stopped="--system stop picoclaw.service
--system disable picoclaw.service
--system stop picoclaw-launcher.service
--system disable picoclaw-launcher.service"

# $1 = label, $2 = expected calls (one per line), $3 = scriptlet argument
expect_calls() {
    : > "$STUB/calls"
    CALLS="$STUB/calls" PATH="$STUB:$PATH" sh "$PREREMOVE" "$3" >/dev/null 2>&1
    got=$(cat "$STUB/calls")
    if [ "$got" = "$2" ]; then
        echo "PASS | preremove '$3' | $1"
        pass=$((pass + 1))
    else
        echo "FAIL | preremove '$3' | $1"
        echo "       expected: [$(printf '%s' "$2" | tr '\n' ';')]"
        echo "       got:      [$(printf '%s' "$got" | tr '\n' ';')]"
        fail=$((fail + 1))
    fi
}

# Upgrade must be a no-op in BOTH vocabularies.
expect_calls "does nothing (RPM upgrade)" "" 1
expect_calls "does nothing (dpkg upgrade)" "" upgrade
expect_calls "does nothing (dpkg failed-upgrade)" "" failed-upgrade
expect_calls "does nothing (dpkg abort-upgrade)" "" abort-upgrade
# Erase must still tear down, in both vocabularies.
expect_calls "stops and disables (RPM erase)" "$both_stopped" 0
expect_calls "stops and disables (dpkg remove)" "$both_stopped" remove

# Every shipped script parses under POSIX sh and dash.
for f in "$SCRIPT_DIR"/install.sh "$SCRIPT_DIR"/scripts/*.sh "$SCRIPT_DIR"/tests/*.sh; do
    for shell in sh dash; do
        command -v "$shell" >/dev/null 2>&1 || continue
        if "$shell" -n "$f" 2>/dev/null; then
            echo "PASS | $shell -n | ${f#"$SCRIPT_DIR"/}"
            pass=$((pass + 1))
        else
            echo "FAIL | $shell -n | ${f#"$SCRIPT_DIR"/}"
            fail=$((fail + 1))
        fi
    done
done

echo
echo "scriptlets: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
