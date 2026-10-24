#!/bin/sh
# PicoClaw installer -- https://github.com/vicowa-botworks/picoclaw
#
# Detects the operating system and CPU architecture, picks the matching release
# asset, verifies its SHA-256 against the release checksums file, and installs
# it. On Linux the .deb/.rpm package is preferred, because that is the only
# route that also installs the systemd units, creates the `picoclaw` user and
# seeds /etc/picoclaw/env. Systems with neither dpkg nor rpm (Alpine, musl,
# slim containers) and every non-Linux system get the tarball.
#
#   curl -fsSL https://raw.githubusercontent.com/vicowa-botworks/picoclaw/main/scripts/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --help
#
# Flags can also be passed as environment variables, which is the only way to
# set them when the script is piped into a shell without `--`:
#
#   PICOCLAW_VERSION=v0.3.2-vicowa.1 curl -fsSL .../install.sh | sh
#
# Written for POSIX sh so that `| sh` works under dash (Debian/Ubuntu /bin/sh),
# busybox ash, ksh and bash alike. No bash-isms, no `local`, no arrays.

set -eu

REPO=${PICOCLAW_REPO:-vicowa-botworks/picoclaw}
VERSION=${PICOCLAW_VERSION:-}
BASE_URL=${PICOCLAW_BASE_URL:-}          # mirror of the release assets
METHOD=${PICOCLAW_METHOD:-auto}          # auto | package | tarball
INSTALL_DIR=${PICOCLAW_INSTALL_DIR:-}    # tarball installs only
DRY_RUN=${PICOCLAW_DRY_RUN:-0}
SKIP_CHECKSUM=${PICOCLAW_SKIP_CHECKSUM:-0}
FORCE=${PICOCLAW_FORCE:-0}
USE_SUDO=${PICOCLAW_SUDO:-auto}          # auto | 0 -> never elevate

TMP=""
FORCE_FLAG=""

cleanup() {
    if [ -n "$TMP" ]; then
        rm -rf "$TMP" 2>/dev/null || true
    fi
}
trap cleanup EXIT INT TERM

# Diagnostics go to stderr so stdout stays clean for callers that wrap this.
say()  { printf '%s\n' "$*" >&2; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

have() { command -v "$1" >/dev/null 2>&1; }

usage() {
    cat <<'EOF'
Install PicoClaw from the GitHub release that matches this machine.

Usage:
  install.sh [options]

Options:
  -v, --version <ver>   Release to install (default: the latest release).
  -m, --method <how>    auto (default), package (deb/rpm) or tarball.
  -d, --install-dir <d> Target directory for tarball installs
                        (default: /usr/local/bin, else ~/.local/bin).
  -f, --force           Reinstall even when that exact version is installed.
  -n, --dry-run         Resolve the platform and print the plan; change nothing.
      --no-checksum     Skip SHA-256 verification (discouraged).
      --no-sudo         Never elevate; fail instead of calling sudo.
  -h, --help            Show this help.

Environment (equivalent to the flags; use these when piping into `sh`):
  PICOCLAW_VERSION, PICOCLAW_METHOD, PICOCLAW_INSTALL_DIR, PICOCLAW_FORCE,
  PICOCLAW_DRY_RUN, PICOCLAW_SKIP_CHECKSUM, PICOCLAW_REPO, PICOCLAW_SUDO,
  PICOCLAW_BASE_URL (a mirror laid out like the release's download dir),
  GITHUB_TOKEN or GH_TOKEN (only sent to api.github.com; raises the rate limit).

Examples:
  curl -fsSL <url> | sh
  curl -fsSL <url> | sh -s -- --version v0.3.2-vicowa.1
  curl -fsSL <url> | sh -s -- --method tarball --install-dir "$HOME/bin"
EOF
}

# ---------------------------------------------------------------- arguments --
while [ $# -gt 0 ]; do
    case $1 in
        -v|--version)     [ $# -ge 2 ] || die "--version needs a value"; VERSION=$2; shift 2 ;;
        -m|--method)      [ $# -ge 2 ] || die "--method needs a value"; METHOD=$2; shift 2 ;;
        -d|--install-dir) [ $# -ge 2 ] || die "--install-dir needs a value"; INSTALL_DIR=$2; shift 2 ;;
        -f|--force)       FORCE=1; shift ;;
        -n|--dry-run)     DRY_RUN=1; shift ;;
        --no-checksum)    SKIP_CHECKSUM=1; shift ;;
        --no-sudo)        USE_SUDO=0; shift ;;
        -h|--help)        usage; exit 0 ;;
        *)                usage >&2; die "unknown option: $1" ;;
    esac
done

case $METHOD in
    auto|package|tarball) ;;
    *) die "--method must be auto, package or tarball (got '$METHOD')" ;;
esac
if [ "$FORCE" = 1 ]; then FORCE_FLAG="--replacepkgs"; fi

# --------------------------------------------------------------- primitives --
# The Authorization header is only ever sent to api.github.com, so a redirect
# off that host can never carry the token along with it.
fetch() { # fetch <url> <dest>
    if have curl; then
        case $1 in
            https://api.github.com/*)
                if [ -n "${GITHUB_TOKEN:-}" ] || [ -n "${GH_TOKEN:-}" ]; then
                    curl -fsSL --retry 3 --retry-delay 1 \
                        -H "Authorization: Bearer ${GITHUB_TOKEN:-$GH_TOKEN}" \
                        -o "$2" "$1" && return 0
                fi ;;
        esac
        curl -fsSL --retry 3 --retry-delay 1 -o "$2" "$1"
    elif have wget; then
        # busybox wget has no --tries, and Alpine's /usr/bin/wget is busybox.
        wget -q -O "$2" "$1"
    else
        die "neither curl nor wget was found; install one and re-run"
    fi
}

sha256_of() { # sha256_of <file> -> hex digest on stdout
    if have sha256sum; then sha256sum "$1" | cut -d' ' -f1
    elif have shasum; then shasum -a 256 "$1" | cut -d' ' -f1
    elif have openssl; then openssl dgst -sha256 "$1" | sed 's/.*= *//'
    elif have certutil; then certutil -hashfile "$1" SHA256 | sed -n '2p' | tr -d ' \r'
    else return 1; fi
}

elevate() { # elevate <what> -- <cmd...> -- run as root, elevating when needed
    # <what> is the human phrase used in the failure message: the command itself
    # is often an `sh -c` body that means nothing to the person installing.
    what=$1
    shift 2
    if [ "$(id -u)" = "0" ]; then
        "$@"
    elif [ "$USE_SUDO" = 0 ]; then
        die "need root privileges to $what (re-run as root, or drop --no-sudo)"
    elif have sudo; then
        sudo "$@"
    else
        die "need root privileges to $what (no sudo found; re-run as root)"
    fi
}

# ------------------------------------------------------- platform detection --
# `title .Os` in the archive name template decides the spelling used by the
# release assets, which is why FreeBSD becomes "Freebsd" and not "FreeBSD".
OS=$(uname -s 2>/dev/null || echo unknown)
HAS_PACKAGES=0
case $OS in
    Linux)   OS_TITLE=Linux;   HAS_PACKAGES=1 ;;
    Darwin)  OS_TITLE=Darwin ;;
    FreeBSD) OS_TITLE=Freebsd ;;
    NetBSD)  OS_TITLE=Netbsd ;;
    MINGW*|MSYS*|CYGWIN*|Windows_NT)
        die "this script does not install on Windows. Download
       picoclaw_Windows_x86_64.zip (or picoclaw_Windows_arm64.zip) from
       https://github.com/$REPO/releases/latest and run picoclaw.exe from it." ;;
    *)
        die "unsupported operating system: $OS
       If it is Linux, macOS, FreeBSD or NetBSD, report this at
       https://github.com/$REPO/issues" ;;
esac

# `uname -m` is spelled differently per kernel and per 32/64-bit userland. Each
# row names both asset suffixes: packages use aarch64 where archives use arm64.
RAW_ARCH=$(uname -m 2>/dev/null || echo unknown)
PKG_ARCH=""
TAR_ARCH=""
NOT_BUILT=""
case $RAW_ARCH in
    x86_64|amd64)                 PKG_ARCH=x86_64;  TAR_ARCH=x86_64 ;;
    aarch64|arm64)                PKG_ARCH=aarch64; TAR_ARCH=arm64 ;;
    armv8l|armv7l|armv7hl|armv7)  PKG_ARCH=armv7;   TAR_ARCH=armv7 ;;
    armv6l|armv6hl|armv6)         PKG_ARCH=armv6;   TAR_ARCH=armv6 ;;
    riscv64)                      PKG_ARCH=riscv64; TAR_ARCH=riscv64 ;;
    loongarch64)                  PKG_ARCH=loong64; TAR_ARCH=loong64 ;;
    s390x)                        PKG_ARCH=s390x;   TAR_ARCH=s390x ;;
    mipsel|mips64el)              PKG_ARCH=mipsle;  TAR_ARCH=mipsle ;;
    mips|mips64)
        # Only little-endian MIPS is built. EI_DATA is byte 5 of any ELF:
        # 01 = little endian, 02 = big endian.
        ENDIAN=""
        for probe in /bin/sh /bin/ls /usr/bin/env; do
            [ -f "$probe" ] || continue
            ENDIAN=$(od -An -j5 -N1 -tx1 "$probe" 2>/dev/null | tr -d ' \n' || true)
            [ -n "$ENDIAN" ] && break
        done
        if [ "$ENDIAN" = "01" ]; then
            PKG_ARCH=mipsle; TAR_ARCH=mipsle
        elif [ "$ENDIAN" = "02" ]; then
            NOT_BUILT="big-endian MIPS ($RAW_ARCH); only the little-endian mipsle build exists"
        else
            # No od, or none of the probe files were readable: we genuinely do
            # not know, so do not blame endianness for a failed probe.
            NOT_BUILT="MIPS ($RAW_ARCH): could not read ELF endianness to tell mipsle from mips; only the little-endian mipsle build exists"
        fi ;;
    armv5*|armv4*|arm)
        NOT_BUILT="$RAW_ARCH (the oldest ARM build is armv6)" ;;
    i386|i486|i586|i686)
        NOT_BUILT="32-bit x86 ($RAW_ARCH)" ;;
esac

if [ -z "$TAR_ARCH" ]; then
    if [ -n "$NOT_BUILT" ]; then
        die "there is no PicoClaw release for $NOT_BUILT on $OS"
    fi
    die "unrecognised CPU architecture: $RAW_ARCH
       Report this at https://github.com/$REPO/issues, or pick the asset by hand
       from https://github.com/$REPO/releases/latest"
fi

# ------------------------------------------------------- install method pick --
# The package is the better answer wherever a package manager can take it: it
# carries the systemd units and runs the maintainer scripts that enable the
# gateway. os-release decides when both tools exist (some Debian systems carry
# rpm for unrelated reasons); tool availability decides when the hint is absent.
PKG_FORMAT=""
if [ "$HAS_PACKAGES" = 1 ] && [ "$METHOD" != tarball ]; then
    DIST_ID=""
    if [ -r /etc/os-release ]; then
        DIST_ID=$(. /etc/os-release 2>/dev/null && printf '%s %s' "${ID:-}" "${ID_LIKE:-}") || true
    fi
    case " $DIST_ID " in
        *" debian "*|*" ubuntu "*|*" devuan "*)
            PKG_FORMAT=deb ;;
        *" rhel "*|*" fedora "*|*" centos "*|*" opensuse "*|*" suse "*|*" almalinux "*|*" rocky "*|*" amzn "*)
            PKG_FORMAT=rpm ;;
    esac
    [ "$PKG_FORMAT" = deb ] && ! have dpkg && PKG_FORMAT=""
    [ "$PKG_FORMAT" = rpm ] && ! have rpm  && PKG_FORMAT=""
    if [ -z "$PKG_FORMAT" ]; then
        if   have dpkg; then PKG_FORMAT=deb
        elif have rpm;  then PKG_FORMAT=rpm
        fi
    fi
fi
if [ "$METHOD" = package ] && [ -z "$PKG_FORMAT" ]; then
    if [ "$HAS_PACKAGES" != 1 ]; then
        die "--method package was requested, but $OS has no .deb/.rpm packages in the release"
    fi
    die "--method package was requested, but neither dpkg nor rpm is available on this system"
fi

if [ -n "$PKG_FORMAT" ]; then
    ASSET="picoclaw_${PKG_ARCH}.${PKG_FORMAT}"
    HOW="package ($PKG_FORMAT)"
else
    ASSET="picoclaw_${OS_TITLE}_${TAR_ARCH}.tar.gz"
    HOW="tarball"
fi

# --------------------------------------------------------------- scratch dir --
TMP=$(mktemp -d 2>/dev/null || true)
if [ -z "$TMP" ]; then
    TMP=${TMPDIR:-/tmp}/picoclaw-install.$$
    mkdir -p "$TMP" || die "cannot create a temporary directory"
fi

# ---------------------------------------------------------- version resolve --
# The checksums file is named after the version, so the version is needed even
# though the asset itself could be fetched from /releases/latest/download/.
resolve_version() {
    # The API works with either downloader, so this path is not curl-only --
    # that matters on Alpine, which ships busybox wget and no curl.
    if fetch "https://api.github.com/repos/$REPO/releases/latest" "$TMP/latest.json" 2>/dev/null; then
        VERSION=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
                  "$TMP/latest.json" 2>/dev/null | head -n 1)
    fi
    # Rate-limited, or the API is unreachable: the HTML endpoint redirects to
    # the tag, so the final URL is the answer.
    if [ -z "$VERSION" ] && have curl; then
        VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
                  "https://github.com/$REPO/releases/latest" 2>/dev/null \
                  | sed -n 's#.*/tag/##p')
    fi
    if [ -z "$VERSION" ] && have wget; then
        VERSION=$(wget --server-response --spider -q \
                  "https://github.com/$REPO/releases/latest" 2>&1 \
                  | sed -n 's/.*[Ll]ocation:.*\/tag\///p' | tr -d '\r' | head -n 1)
    fi
    [ -n "$VERSION" ]
}

if [ -z "$VERSION" ]; then
    if ! resolve_version; then
        # A dry run must still be useful with no network at all.
        if [ "$DRY_RUN" = 1 ]; then
            VERSION='<latest>'
        else
            die "could not determine the latest release of $REPO.
       Name one explicitly: --version v0.3.2-vicowa.1"
        fi
    fi
fi

# --------------------------------------------------------------- install dir --
pick_install_dir() {
    [ -n "$INSTALL_DIR" ] && return 0
    if [ "$(id -u)" = "0" ] || [ -w /usr/local/bin ]; then
        INSTALL_DIR=/usr/local/bin
    elif [ "$USE_SUDO" != 0 ] && have sudo; then
        INSTALL_DIR=/usr/local/bin
    else
        INSTALL_DIR=${HOME:-.}/.local/bin
    fi
}

say "Platform : $OS $RAW_ARCH  ->  $OS_TITLE/$TAR_ARCH"
say "Release  : $VERSION"
say "Method   : $HOW"
say "Asset    : $ASSET"

if [ "$DRY_RUN" = 1 ]; then
    if [ "$HOW" = tarball ]; then
        pick_install_dir
        say "Installs : $ASSET -> $INSTALL_DIR"
    fi
    say "dry run: nothing was downloaded and nothing was installed."
    exit 0
fi

# ------------------------------------------------------------------ download --
# PICOCLAW_BASE_URL points at a mirror that lays its files out exactly like a
# GitHub release directory (same asset names, same checksums file). The
# checksums file is still fetched from that base and still enforced, so a
# mirror cannot slip a different binary past this script unnoticed.
[ -n "$BASE_URL" ] || BASE_URL="https://github.com/$REPO/releases/download/$VERSION"
say "Downloading $BASE_URL/$ASSET"
fetch "$BASE_URL/$ASSET" "$TMP/$ASSET" || die "download failed: $BASE_URL/$ASSET"

# --------------------------------------------------------------- integrity ---
# Guards against a truncated download and against pulling the wrong asset. It
# is not a signature: no release asset is signed, so this cannot prove the
# release itself is genuine -- only that the bytes match what the release says.
if [ "$SKIP_CHECKSUM" = 1 ]; then
    warn "SHA-256 verification skipped (--no-checksum)"
else
    if ! sha256_of "$TMP/$ASSET" >/dev/null 2>&1; then
        die "no SHA-256 tool found (need sha256sum, shasum, openssl or certutil).
       Re-run with --no-checksum only if you accept the risk."
    fi
    # goreleaser names the checksums file after .Version, and .Version has no
    # leading "v": tag v0.3.2-vicowa.1 publishes
    # picoclaw_0.3.2-vicowa.1_checksums.txt, not picoclaw_v0.3.2-...
    # The unstripped name is tried second so a mirror that keeps the tag form
    # still works.
    SUMFILE=""
    for candidate in "picoclaw_${VERSION#v}_checksums.txt" "picoclaw_${VERSION}_checksums.txt"; do
        if fetch "$BASE_URL/$candidate" "$TMP/checksums.txt" 2>/dev/null; then
            SUMFILE=$candidate
            break
        fi
    done
    [ -n "$SUMFILE" ] || die "could not download the checksums file for $VERSION"
    WANT=$(awk -v f="$ASSET" '{ n = $2; sub(/^\*/, "", n); if (n == f) { print $1; exit } }' \
           "$TMP/checksums.txt" 2>/dev/null || true)
    [ -n "$WANT" ] || die "no checksum entry for $ASSET in the release checksums file"
    GOT=$(sha256_of "$TMP/$ASSET")
    WANT=$(printf '%s' "$WANT" | tr 'A-F' 'a-f')
    GOT=$(printf '%s' "$GOT" | tr 'A-F' 'a-f')
    [ "$WANT" = "$GOT" ] || die "SHA-256 mismatch for $ASSET
       expected $WANT
       got      $GOT
       The download is corrupt or the release asset was altered. Aborting."
    say "Verified : SHA-256 $GOT"
fi

# ---------------------------------------------------------- package install --
if [ -n "$PKG_FORMAT" ]; then
    # Compare against the version inside the downloaded package rather than
    # re-deriving nfpm's own version rewriting (it renders 0.3.2-vicowa.1 as
    # 0.3.2~vicowa.1 for dpkg, and splits it across Version/Release for rpm).
    if [ "$PKG_FORMAT" = deb ]; then
        PKG_VER=$(dpkg-deb -f "$TMP/$ASSET" Version 2>/dev/null || true)
        INSTALLED=$(dpkg-query -W -f='${Version}' picoclaw 2>/dev/null || true)
    else
        PKG_VER=$(rpm -qp --qf '%{VERSION}-%{RELEASE}' "$TMP/$ASSET" 2>/dev/null || true)
        INSTALLED=$(rpm -q --qf '%{VERSION}-%{RELEASE}' picoclaw 2>/dev/null || true)
    fi

    if [ -n "$PKG_VER" ] && [ "$PKG_VER" = "$INSTALLED" ] && [ "$FORCE" != 1 ]; then
        say "PicoClaw $PKG_VER is already installed -- nothing to do (use --force to reinstall)."
        exit 0
    fi

    if [ "$PKG_FORMAT" = deb ]; then
        elevate "install the .deb with dpkg" -- dpkg -i "$TMP/$ASSET" \
            || die "dpkg -i failed; re-run as root to see the full error"
    else
        # shellcheck disable=SC2086  # FORCE_FLAG is intentionally word-split
        elevate "install the .rpm with rpm" -- rpm -Uvh $FORCE_FLAG "$TMP/$ASSET" \
            || die "rpm -Uvh failed; re-run as root to see the full error"
    fi
    BINARY=$(command -v picoclaw || true)
    [ -n "$BINARY" ] || BINARY=/usr/bin/picoclaw
else
    # --------------------------------------------------------- tarball install --
    pick_install_dir
    say "Installing -> $INSTALL_DIR"
    mkdir -p "$TMP/unpacked"
    # These archives keep their files at the top level, so unpack into a private
    # directory instead of wherever the caller happens to be standing.
    ( cd "$TMP/unpacked" && tar xzf "$TMP/$ASSET" ) || die "could not unpack $ASSET"
    for bin in picoclaw picoclaw-launcher; do
        [ -f "$TMP/unpacked/$bin" ] || die "$ASSET did not contain $bin"
    done
    # Only elevate when the target actually needs it. Installing into a
    # directory the caller already owns must not prompt for a password -- and
    # under `curl | sh` a needless sudo prompt looks like a hang.
    if [ -w "$INSTALL_DIR" ] || mkdir -p "$INSTALL_DIR" 2>/dev/null; then
        for bin in picoclaw picoclaw-launcher; do
            install -m 0755 "$TMP/unpacked/$bin" "$INSTALL_DIR/$bin" \
                || die "could not install into $INSTALL_DIR"
        done
    else
        elevate "install binaries into $INSTALL_DIR" -- sh -c \
            'mkdir -p "$1" && for b in picoclaw picoclaw-launcher; do
                 install -m 0755 "$2/$b" "$1/$b"
             done' sh "$INSTALL_DIR" "$TMP/unpacked" \
            || die "could not install into $INSTALL_DIR"
    fi
    BINARY="$INSTALL_DIR/picoclaw"
fi

# ------------------------------------------------------------------ verify ---
[ -x "$BINARY" ] || die "$BINARY is missing or not executable after installing"
if ! VER_OUT=$("$BINARY" version 2>&1); then
    die "PicoClaw installed, but \`$BINARY version\` failed to run"
fi
# `picoclaw version` opens with a banner, so the version line has to be found
# rather than assumed to be first.
VER_LINE=$(printf '%s\n' "$VER_OUT" | tr -d '\r' \
    | grep -E 'picoclaw[[:space:]]+[^[:space:]]+' | head -n 1 \
    | sed 's/^[[:space:]]*//')
[ -n "$VER_LINE" ] || VER_LINE=$(printf '%s\n' "$VER_OUT" | tr -d '\r' \
    | grep -v '^[[:space:]]*$' | head -n 1)
say "Installed: ${VER_LINE:-picoclaw (version string not reported)}"

if [ "$HOW" = tarball ]; then
    case ":$PATH:" in
        *":$INSTALL_DIR:"*) ;;
        *) warn "$INSTALL_DIR is not in PATH; add it with:
       export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
    esac
fi

if [ "$HOW" != tarball ] && have systemctl; then
    if [ "$(systemctl is-enabled picoclaw 2>/dev/null || true)" = "enabled" ]; then
        say "Service  : picoclaw.service is enabled and will start on boot."
        say "           Start it now with: sudo systemctl start picoclaw"
    fi
fi

say ""
say "Next steps:"
say "  picoclaw \"what is the time?\"     ask a one-shot question"
say "  picoclaw                          interactive session"
say "  picoclaw-launcher                 web UI on http://localhost:18800"
