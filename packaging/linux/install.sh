#!/bin/sh
# Install PicoClaw on a Linux host and wire it up to start at boot. Validated on
# Raspberry Pi OS and other 64-bit ARM Debian/Ubuntu systems; the units are
# architecture-neutral.
#
# Use this when you have the raw binaries / tar.gz. If you have the .deb, you do
# not need this: `sudo dpkg -i picoclaw_aarch64.deb` already does everything
# below, including creating the service account and enabling the unit.
#
#   sudo ./install.sh [--bin-dir DIR] [--with-launcher]
#
#   --bin-dir DIR     where picoclaw and picoclaw-launcher live (default: this
#                     script's directory, or its parent for the release bundle)
#   --with-launcher   also enable picoclaw-launcher.service at boot
#                     (default: installed but disabled)
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

# Binaries sit next to this script when you run it out of a plain checkout, and
# one level up in the release bundle (where the service files live in service/).
# --bin-dir overrides either way.
BIN_DIR=""
for d in "$SCRIPT_DIR" "$SCRIPT_DIR/.."; do
    if [ -x "$d/picoclaw" ]; then
        BIN_DIR=$(CDPATH= cd -- "$d" && pwd)
        break
    fi
done
BIN_DIR="${BIN_DIR:-$SCRIPT_DIR}"
ENABLE_LAUNCHER=0

while [ $# -gt 0 ]; do
    case "$1" in
        --bin-dir)
            [ $# -ge 2 ] || { echo "install.sh: --bin-dir needs a value" >&2; exit 2; }
            BIN_DIR=$2
            shift 2
            ;;
        --with-launcher)
            ENABLE_LAUNCHER=1
            shift
            ;;
        -h | --help)
            sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "install.sh: unknown argument: $1" >&2
            exit 2
            ;;
    esac
done

if [ "$(id -u)" -ne 0 ]; then
    echo "install.sh: must run as root (sudo $0)" >&2
    exit 1
fi

for f in systemd/picoclaw.service env.example scripts/preinstall.sh; do
    if [ ! -f "$SCRIPT_DIR/$f" ]; then
        echo "install.sh: $f missing next to this script — run it from the bundle dir" >&2
        exit 1
    fi
done

if [ ! -x "$BIN_DIR/picoclaw" ]; then
    echo "install.sh: no executable picoclaw in $BIN_DIR (use --bin-dir)" >&2
    exit 1
fi

# Rather than trusting `uname`, ask the kernel whether the binary actually
# executes. That catches the real mistake (an arm64 bundle on an x86 board, or
# 64-bit binaries on a 32-bit Pi OS) and still lets you run this on whatever
# host you are actually preparing.
if ! "$BIN_DIR/picoclaw" version >/dev/null 2>&1; then
    echo "install.sh: $BIN_DIR/picoclaw will not execute on this host" >&2
    echo "               (host is $(uname -m)). Wrong bundle architecture?" >&2
    exit 1
fi

echo "==> creating service account and state directories"
sh "$SCRIPT_DIR/scripts/preinstall.sh"

echo "==> installing binaries"
install -m 0755 "$BIN_DIR/picoclaw" /usr/bin/picoclaw
if [ -x "$BIN_DIR/picoclaw-launcher" ]; then
    install -m 0755 "$BIN_DIR/picoclaw-launcher" /usr/bin/picoclaw-launcher
else
    echo "    (no picoclaw-launcher in $BIN_DIR — skipping the console binary)"
fi

echo "==> installing systemd units"
install -d -m 0755 /etc/systemd/system
install -m 0644 "$SCRIPT_DIR/systemd/picoclaw.service" /etc/systemd/system/picoclaw.service
if [ -f "$SCRIPT_DIR/systemd/picoclaw-launcher.service" ] && [ -x /usr/bin/picoclaw-launcher ]; then
    install -m 0644 "$SCRIPT_DIR/systemd/picoclaw-launcher.service" \
        /etc/systemd/system/picoclaw-launcher.service
fi

if [ ! -f /etc/picoclaw/env ]; then
    echo "==> installing /etc/picoclaw/env from the template (edit it, then restart)"
    install -m 0640 -o root -g picoclaw "$SCRIPT_DIR/env.example" /etc/picoclaw/env
else
    echo "==> keeping your existing /etc/picoclaw/env"
fi

has_systemctl() { command -v systemctl >/dev/null 2>&1; }

if has_systemctl; then
    # Both are best-effort: this script is also how people install into a chroot
    # while building an SD-card image (pi-gen and friends), where there is no
    # running systemd to reload or enable against. The symlinks it creates still
    # take effect on first boot.
    systemctl daemon-reload >/dev/null 2>&1 || true
    if systemctl enable picoclaw.service >/dev/null 2>&1; then
        ENABLED=yes
    else
        ENABLED=no
    fi
    if [ "$ENABLE_LAUNCHER" -eq 1 ] && [ -x /usr/bin/picoclaw-launcher ]; then
        systemctl enable picoclaw-launcher.service >/dev/null 2>&1 || true
    fi
else
    # No systemd at all. Recreate the boot symlink by hand so the unit is still
    # enabled when the board first boots.
    mkdir -p /etc/systemd/system/multi-user.target.wants
    ln -sfn /etc/systemd/system/picoclaw.service \
        /etc/systemd/system/multi-user.target.wants/picoclaw.service
    ENABLED=symlink-only
fi

echo
if [ "$ENABLED" = yes ]; then
    echo "Installed. picoclaw.service is enabled at boot but will not start yet:"
    echo "its ConditionPathExists=/var/lib/picoclaw/config.json is false until you"
    echo "configure a model. Do that now:"
else
    echo "Installed, but systemd could not be reached, so it was NOT started and"
    echo "enablement may be symlink-only. Enable it yourself once you are on the"
    echo "running system:  sudo systemctl enable picoclaw.service"
    echo "Then configure a model:"
fi
echo
echo "    sudo -u picoclaw PICOCLAW_HOME=/var/lib/picoclaw picoclaw onboard"
echo "    sudoedit /etc/picoclaw/env        # put your API key here, not in config.json"
echo "    sudo systemctl start picoclaw"
echo "    systemctl status picoclaw --no-pager"
echo
echo "Or skip the CLI and drive setup from a browser:"
echo
echo "    sudo systemctl start picoclaw-launcher"
echo "    ssh -L 18800:localhost:18800 <user>@\$(hostname -I | awk '{print \$1}')"
echo "    # then open http://localhost:18800 on your laptop"
