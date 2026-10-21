#!/usr/bin/env bash
# Test suite for scripts/install.sh (the one-line installer).
#
#     ./scripts/tests/installer-suite.sh            # everything
#     ./scripts/tests/installer-suite.sh --no-docker # skip the container installs
#
# Sections:
#   1 static      parses under bash, dash and busybox ash
#   2 matrix      every `uname` spelling the script maps resolves to an asset
#                 that really exists in the published release (needs network)
#   3 negative    unsupported OS/arch/flag combinations fail with a message
#   4 mirror      download + SHA-256 verification + tarball install against a
#                 local mirror, including the mismatch and missing-file paths
#   5 pipe        the documented `curl ... | sh -s -- <flags>` shape
#   6 containers  real .deb / .rpm / tarball installs in debian, fedora, alpine,
#                 as root and as a sudo user (needs docker)
#
# Section 6 is the only one that exercises the package path end to end, and the
# debian case runs in a BOOTED systemd container so that "the installer leaves
# the gateway enabled" is a measured claim. Same rule as container-suite.sh:
# run this natively on x86_64, never under qemu.
set -uo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
INSTALLER="$REPO_ROOT/scripts/install.sh"
W=$(mktemp -d)
CONTAINERS=()
HTTPD=""
USE_DOCKER=1
[ "${1:-}" = "--no-docker" ] && USE_DOCKER=0

cleanup() {
    [ -n "$HTTPD" ] && kill "$HTTPD" 2>/dev/null
    for c in ${CONTAINERS[@]+"${CONTAINERS[@]}"}; do
        docker rm -f "$c" >/dev/null 2>&1
    done
    rm -rf "$W"
}
trap cleanup EXIT

PASS=0
FAIL=0
SKIP=0
chk() { # chk <name> <expected> <actual>
    if [ "$2" = "$3" ]; then echo "PASS | $1 | $3"; PASS=$((PASS + 1));
    else echo "FAIL | $1 | expected [$2] got [$3]"; FAIL=$((FAIL + 1)); fi
}
chk_ne() { # chk_ne <name> <not-expected> <actual>
    if [ "$2" != "$3" ]; then echo "PASS | $1 | $3"; PASS=$((PASS + 1));
    else echo "FAIL | $1 | must not be [$2]"; FAIL=$((FAIL + 1)); fi
}
skip() { echo "SKIP | $1 | $2"; SKIP=$((SKIP + 1)); }
head_() { echo; echo "=== $* ==="; }

[ -f "$INSTALLER" ] || { echo "installer not found at $INSTALLER" >&2; exit 2; }

# The release under test: the published one by default, so section 2 checks the
# script against the assets users actually receive.
VERSION=${PICOCLAW_TEST_VERSION:-v0.3.2-vicowa.1}
GH_REPO=${PICOCLAW_TEST_REPO:-vicowa-botworks/picoclaw}
BASE="https://github.com/$GH_REPO/releases/download/$VERSION"

# ---------------------------------------------------------------- 1. static --
head_ "1. static: parses under every shell the installer claims to support"
chk "installer is executable" "yes" "$([ -x "$INSTALLER" ] && echo yes || echo no)"
chk "bash -n" "0" "$(bash -n "$INSTALLER" 2>/dev/null; echo $?)"
if command -v dash >/dev/null 2>&1; then
    chk "dash -n" "0" "$(dash -n "$INSTALLER" 2>/dev/null; echo $?)"
else skip "dash -n" "dash not installed"; fi
if command -v busybox >/dev/null 2>&1; then
    chk "busybox ash -n" "0" "$(busybox sh -n "$INSTALLER" 2>/dev/null; echo $?)"
else skip "busybox ash -n" "busybox not installed"; fi
# A bash-only construct would parse above but fail here; grep is the cheap net.
# A bash-only construct would parse fine under dash above, so grep is the net.
# `\[\[ ` (with the space) is a bash conditional; `[[:space:]]` is a POSIX class
# and must not trip it.
BASHISM=$(grep -nE '(\[\[ |<<<|\blocal |\bfunction |\$\(\(|\+=|\bdeclare |\$\{[A-Za-z_][A-Za-z0-9_]*[/^%])' "$INSTALLER" | grep -v '^[0-9]*:[[:space:]]*#' || true)
chk "no bash-isms in a POSIX-sh script" "" "$BASHISM"

# --------------------------------------------------------------- 2. matrix ---
# A fake `uname` lets one host stand in for all eight architectures and five
# kernels. Every asset name the script produces is then required to exist.
head_ "2. matrix: uname spellings -> published assets"
BIN="$W/bin"; mkdir -p "$BIN"
cat > "$BIN/uname" <<'EOS'
#!/bin/sh
case "${1:-}" in
    -s) printf '%s\n' "$FAKE_UNAME_S" ;;
    -m) printf '%s\n' "$FAKE_UNAME_M" ;;
    *)  exec /usr/bin/uname "$@" ;;
esac
EOS
chmod 0755 "$BIN/uname"

# The installer decides big- vs little-endian MIPS by reading EI_DATA, byte 5 of
# any ELF. Faking `od` lets this one little-endian host stand in for a big-endian
# MIPS machine too; without it the big-endian branch is untestable and the
# `mips` row silently measures this host's endianness instead.
cat > "$BIN/od" <<'EOS'
#!/bin/sh
if [ -n "${FAKE_EI_DATA:-}" ]; then
    [ "$FAKE_EI_DATA" = ERR ] && exit 1
    printf '%s\n' "$FAKE_EI_DATA"; exit 0
fi
exec /usr/bin/od "$@"
EOS
chmod 0755 "$BIN/od"

if curl -o /dev/null -sIL -w '%{http_code}' "$BASE/picoclaw_${VERSION#v}_checksums.txt" | grep -q '^200$'; then
    ONLINE=1
else
    ONLINE=0
    skip "matrix" "no network access to $BASE -- arch->asset mapping is UNTESTED this run"
fi

asset_of() { # asset_of <os> <arch> <method> [ei_data] -> asset name from a dry run
    FAKE_UNAME_S=$1 FAKE_UNAME_M=$2 FAKE_EI_DATA=${4:-} PATH="$BIN:$PATH" \
        dash "$INSTALLER" --dry-run --method "$3" --version "$VERSION" 2>&1 \
        | sed -n 's/^Asset    : //p'
}
http_code() { curl -o /dev/null -sIL -w '%{http_code}' "$1"; }

if [ "$ONLINE" = 1 ]; then
    # The checksums file is named after .Version, not after the tag. Assert the
    # name the installer derives is the name the release actually publishes.
    chk "checksums asset the installer derives exists" "200" \
        "$(http_code "$BASE/picoclaw_${VERSION#v}_checksums.txt")"

    # arch: uname spelling | expected tar arch | expected package arch
    for row in \
        "x86_64|x86_64|x86_64" \
        "amd64|x86_64|x86_64" \
        "aarch64|arm64|aarch64" \
        "arm64|arm64|aarch64" \
        "armv8l|armv7|armv7" \
        "armv7l|armv7|armv7" \
        "armv7hl|armv7|armv7" \
        "armv7|armv7|armv7" \
        "armv6l|armv6|armv6" \
        "armv6hl|armv6|armv6" \
        "riscv64|riscv64|riscv64" \
        "loongarch64|loong64|loong64" \
        "s390x|s390x|s390x" \
        "mipsel|mipsle|mipsle" \
        "mips64el|mipsle|mipsle" \
        "mips|mipsle|mipsle|01"
    do
        IFS='|' read -r raw tarex pkgex ei <<< "$row"
        tar_asset=$(asset_of Linux "$raw" tarball "$ei")
        deb_asset=$(asset_of Linux "$raw" package "$ei")
        chk "arch $raw -> tarball" "picoclaw_Linux_$tarex.tar.gz" "$tar_asset"
        chk "arch $raw -> package" "picoclaw_$pkgex.deb" "$deb_asset"
        chk "asset exists: $tar_asset" "200" "$(http_code "$BASE/$tar_asset")"
        chk "asset exists: $deb_asset" "200" "$(http_code "$BASE/$deb_asset")"
        chk "asset exists: picoclaw_$pkgex.rpm" "200" "$(http_code "$BASE/picoclaw_$pkgex.rpm")"
    done

    # Non-Linux kernels have tarballs only, and `auto` must know that.
    for os in Darwin FreeBSD NetBSD; do
        case $os in Darwin) title=Darwin;; FreeBSD) title=Freebsd;; NetBSD) title=Netbsd;; esac
        a=$(asset_of "$os" x86_64 auto)
        chk "os $os auto -> tarball" "picoclaw_${title}_x86_64.tar.gz" "$a"
        chk "asset exists: $a" "200" "$(http_code "$BASE/$a")"
        a=$(asset_of "$os" aarch64 auto)
        chk "os $os arm64 auto -> tarball" "picoclaw_${title}_arm64.tar.gz" "$a"
        chk "asset exists: $a" "200" "$(http_code "$BASE/$a")"
    done
fi

# ------------------------------------------------------------- 3. negative ---
head_ "3. negative: unsupported platforms fail loudly instead of guessing"
neg() { # neg <name> <os> <arch> <method> <substring> [ei_data]
    out=$(FAKE_UNAME_S=$2 FAKE_UNAME_M=$3 FAKE_EI_DATA=${6:-} PATH="$BIN:$PATH" \
          dash "$INSTALLER" --dry-run --method "$4" --version "$VERSION" 2>&1)
    rc=$?
    chk "$1 exits non-zero" "nonzero" "$([ "$rc" -ne 0 ] && echo nonzero || echo zero)"
    case $out in *"$5"*) chk "$1 explains why" "yes" "yes" ;;
                   *)   chk "$1 explains why" "yes containing [$5]" "no: $out" ;; esac
}
neg "i686"            Linux i686  tarball "32-bit x86"
neg "i386"            Linux i386  tarball "32-bit x86"
neg "armv5tel"        Linux armv5tel tarball "oldest ARM build is armv6"
neg "arm"             Linux arm   tarball "oldest ARM build is armv6"
neg "sparc64"         Linux sparc64 tarball "unrecognised CPU architecture"
neg "ppc64le"         Linux ppc64le tarball "unrecognised CPU architecture"
neg "riscv32"         Linux riscv32 tarball "unrecognised CPU architecture"
# The same uname string is little- or big-endian depending on the CPU; byte 5 of
# an ELF is the only way to tell, and only mipsle is built.
neg "mips big-endian" Linux mips    tarball "big-endian MIPS" 02
neg "mips64 big-endian" Linux mips64 tarball "big-endian MIPS" 02
neg "mips endian probe failed" Linux mips tarball "could not read ELF endianness" ERR
neg "SunOS"           SunOS sparc auto "unsupported operating system"
neg "AIX"             AIX powerpc auto "unsupported operating system"
neg "Windows (msys)"  MINGW64_NT-10.0-19045 x86_64 auto "does not install on Windows"
neg "Windows (nt)"    Windows_NT x86_64 auto "does not install on Windows"
neg "package on mac"  Darwin arm64 package "no .deb/.rpm packages"

out=$(dash "$INSTALLER" --method bogus --dry-run 2>&1); rc=$?
chk "--method bogus rejected" "nonzero" "$([ "$rc" -ne 0 ] && echo nonzero || echo zero)"
case $out in *"must be auto, package or tarball"*) chk "--method bogus explained" yes yes ;;
             *) chk "--method bogus explained" yes no ;; esac
out=$(dash "$INSTALLER" --nope 2>&1); rc=$?
chk "unknown flag rejected" "nonzero" "$([ "$rc" -ne 0 ] && echo nonzero || echo zero)"
out=$(dash "$INSTALLER" --version 2>&1); rc=$?
chk "--version with no value rejected" "nonzero" "$([ "$rc" -ne 0 ] && echo nonzero || echo zero)"

# Big-endian MIPS must be refused. The probe reads EI_DATA out of an ELF, so
# check the probe itself against a synthesised big-endian header.
printf '\177ELF\002\002\001' > "$W/be.elf"
ENDIAN=$(od -An -j5 -N1 -tx1 "$W/be.elf" | tr -d ' \n')
chk "ELF endianness probe reads big endian as 02" "02" "$ENDIAN"
printf '\177ELF\002\001\001' > "$W/le.elf"
chk "ELF endianness probe reads little endian as 01" "01" \
    "$(od -An -j5 -N1 -tx1 "$W/le.elf" | tr -d ' \n')"

# --------------------------------------------------------------- 4. mirror ---
head_ "4. mirror: download, SHA-256 verification, tarball install"
# A stand-in release directory, laid out exactly like a GitHub release, served
# over http. This is what makes the integrity path testable at all: the real
# release cannot be corrupted on purpose.
MIR="$W/mirror"; mkdir -p "$MIR"
STAGE="$W/stage"; mkdir -p "$STAGE"
mk_stub() { # mk_stub <name>
    printf '#!/bin/sh\nif [ "$1" = version ]; then echo "picoclaw 9.9.9-test (git: stub)"; exit 0; fi\nexit 0\n' \
        > "$STAGE/$1"; chmod 0755 "$STAGE/$1"; }
mk_stub picoclaw; mk_stub picoclaw-launcher
printf 'MIT\n' > "$STAGE/LICENSE"
( cd "$STAGE" && tar czf "$MIR/picoclaw_Linux_x86_64.tar.gz" picoclaw picoclaw-launcher LICENSE )
( cd "$MIR" && sha256sum picoclaw_Linux_x86_64.tar.gz > picoclaw_9.9.9-test_checksums.txt )

python3 -m http.server --help >/dev/null 2>&1 && HAVE_PY=1 || HAVE_PY=0
if [ "$HAVE_PY" = 1 ]; then
    PORT=$((18000 + RANDOM % 500))
    ( cd "$MIR" && exec python3 -m http.server "$PORT" --bind 127.0.0.1 ) >"$W/httpd.log" 2>&1 &
    HTTPD=$!
    for _ in 1 2 3 4 5 6 7 8 9 10; do
        curl -fsS -o /dev/null "http://127.0.0.1:$PORT/" 2>/dev/null && break
        sleep 0.5
    done
    MBASE="http://127.0.0.1:$PORT"
    run_mirror() { # run_mirror <args...>
        # PICOCLAW_SUDO=0 so that any needless elevation is a hard failure here
        # instead of a sudo password prompt waiting on a terminal.
        # The version is tag-shaped (leading v) while the checksums file is not,
        # which is what goreleaser actually publishes and what used to break.
        PICOCLAW_BASE_URL=$MBASE PICOCLAW_VERSION=v9.9.9-test PICOCLAW_SUDO=0 \
            sh "$INSTALLER" --method tarball "$@" 2>&1
    }

    DEST="$W/prefix"
    out=$(run_mirror --install-dir "$DEST"); rc=$?
    chk "mirror install exits 0" "0" "$rc"
    case $out in *"Verified : SHA-256"*) chk "good checksum accepted" yes yes ;;
                 *) chk "good checksum accepted" yes "no: $out" ;; esac
    chk "picoclaw installed" "755" "$(stat -c %a "$DEST/picoclaw" 2>/dev/null)"
    chk "picoclaw-launcher installed" "755" "$(stat -c %a "$DEST/picoclaw-launcher" 2>/dev/null)"
    chk "installed binary runs" "picoclaw 9.9.9-test (git: stub)" "$("$DEST/picoclaw" version)"
    chk "version echoed back by installer" "yes" \
        "$(case $out in *'Installed: picoclaw 9.9.9-test'*) echo yes;; *) echo no;; esac)"
    chk "LICENSE not dumped next to the binaries" "no" \
        "$([ -e "$DEST/LICENSE" ] && echo yes || echo no)"
    case $out in *"is not in PATH"*) chk "PATH warning shown" yes yes ;;
                  *) chk "PATH warning shown" yes "no: $out" ;; esac

    # Corrupt the checksums entry: the install must stop before touching disk.
    # A fixed all-zero digest, so the corruption cannot accidentally be a no-op.
    sed 's/^[0-9a-f]\{64\}/0000000000000000000000000000000000000000000000000000000000000000/' \
        "$MIR/picoclaw_9.9.9-test_checksums.txt" > "$W/bad.txt"
    cp "$MIR/picoclaw_9.9.9-test_checksums.txt" "$W/good.txt"
    cp "$W/bad.txt" "$MIR/picoclaw_9.9.9-test_checksums.txt"
    DEST2="$W/prefix2"; out=$(run_mirror --install-dir "$DEST2"); rc=$?
    chk "checksum mismatch exits non-zero" "nonzero" "$([ "$rc" -ne 0 ] && echo nonzero || echo zero)"
    case $out in *"SHA-256 mismatch"*) chk "mismatch is explained" yes yes ;;
                 *) chk "mismatch is explained" yes "no: $out" ;; esac
    chk "nothing installed after a mismatch" "no" "$([ -e "$DEST2/picoclaw" ] && echo yes || echo no)"
    cp "$W/good.txt" "$MIR/picoclaw_9.9.9-test_checksums.txt"

    # --no-checksum must still install, and say that it skipped.
    DEST3="$W/prefix3"; out=$(run_mirror --install-dir "$DEST3" --no-checksum); rc=$?
    chk "--no-checksum installs" "0" "$rc"
    case $out in *"verification skipped"*) chk "--no-checksum warns" yes yes ;;
                 *) chk "--no-checksum warns" yes "no: $out" ;; esac

    # Missing checksums file, and a tarball missing a binary.
    mv "$MIR/picoclaw_9.9.9-test_checksums.txt" "$W/hidden.txt"
    out=$(run_mirror --install-dir "$W/prefix4"); rc=$?
    chk "missing checksums file fails" "nonzero" "$([ "$rc" -ne 0 ] && echo nonzero || echo zero)"
    case $out in *"could not download the checksums file"*) chk "missing checksums explained" yes yes ;;
                 *) chk "missing checksums explained" yes "no: $out" ;; esac
    mv "$W/hidden.txt" "$MIR/picoclaw_9.9.9-test_checksums.txt"

    ( cd "$STAGE" && rm -f "$MIR/partial.tar.gz" && tar czf "$MIR/partial.tar.gz" picoclaw )
    cp "$MIR/partial.tar.gz" "$MIR/picoclaw_Linux_x86_64.tar.gz.tmp"
    ( cd "$MIR" && mv picoclaw_Linux_x86_64.tar.gz picoclaw_Linux_x86_64.tar.gz.full \
        && mv picoclaw_Linux_x86_64.tar.gz.tmp picoclaw_Linux_x86_64.tar.gz \
        && sha256sum picoclaw_Linux_x86_64.tar.gz > picoclaw_9.9.9-test_checksums.txt )
    out=$(run_mirror --install-dir "$W/prefix5"); rc=$?
    chk "tarball without picoclaw-launcher fails" "nonzero" \
        "$([ "$rc" -ne 0 ] && echo nonzero || echo zero)"
    case $out in *"did not contain picoclaw-launcher"*) chk "missing binary explained" yes yes ;;
                 *) chk "missing binary explained" yes "no: $out" ;; esac
    ( cd "$MIR" && mv picoclaw_Linux_x86_64.tar.gz.full picoclaw_Linux_x86_64.tar.gz \
        && sha256sum picoclaw_Linux_x86_64.tar.gz > picoclaw_9.9.9-test_checksums.txt )
    rm -f "$MIR/partial.tar.gz"
else
    skip "mirror section" "python3 not available"
fi

# ----------------------------------------------------------------- 5. pipe ---
head_ "5. pipe: the documented curl | sh shape"
# Piping the script into a shell while passing flags after `--` is exactly what
# the README tells users to run; a stray stdin read would eat the script here.
out=$(sh -s -- --dry-run --version "$VERSION" --method tarball < "$INSTALLER" 2>&1); rc=$?
chk "sh -s -- <flags> < script" "0" "$rc"
case $out in *"dry run: nothing was downloaded"*) chk "piped dry run completes" yes yes ;;
             *) chk "piped dry run completes" yes "no: $out" ;; esac
out=$(sh -s -- --help < "$INSTALLER" 2>&1); rc=$?
chk "piped --help" "0" "$rc"
out=$(PICOCLAW_DRY_RUN=1 PICOCLAW_VERSION="$VERSION" PICOCLAW_METHOD=tarball \
      sh < "$INSTALLER" 2>&1); rc=$?
chk "env-var form of the flags" "0" "$rc"
case $out in *"Method   : tarball"*) chk "env PICOCLAW_METHOD honoured" yes yes ;;
             *) chk "env PICOCLAW_METHOD honoured" yes "no: $out" ;; esac
out=$(dash "$INSTALLER" --dry-run 2>&1)
chk "latest release resolves with no --version" "$VERSION" \
    "$(printf '%s\n' "$out" | sed -n 's/^Release  : //p')"

# ------------------------------------------------------------ 6. containers --
head_ "6. containers: real installs"
if [ "$USE_DOCKER" = 0 ]; then
    skip "container section" "--no-docker"
elif ! command -v docker >/dev/null 2>&1; then
    skip "container section" "docker not installed"
else
    # Both package tests boot a real systemd as PID 1: the deb/rpm maintainer
    # scripts call systemctl, and "the installer leaves the gateway enabled" is
    # only a measured claim if systemd was actually running.
    cat > "$W/Dockerfile.debian" <<'EOF'
FROM debian:trixie-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get -y --no-install-recommends install \
      systemd systemd-sysv dbus curl ca-certificates sudo \
 && rm -rf /var/lib/apt/lists/*
RUN useradd -m -s /bin/bash tester \
 && echo 'tester ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/tester
STOPSIGNAL SIGRTMIN+3
CMD ["/lib/systemd/systemd"]
EOF
    cat > "$W/Dockerfile.fedora" <<'EOF'
FROM fedora:42
RUN dnf -y install systemd curl >/dev/null 2>&1 || true
STOPSIGNAL SIGRTMIN+3
CMD ["/usr/lib/systemd/systemd"]
EOF
    cat > "$W/Dockerfile.alpine" <<'EOF'
FROM alpine:latest
RUN apk add --no-cache ca-certificates >/dev/null 2>&1 || true
EOF

    echo "==> building images (slow the first time)"
    docker build -q -f "$W/Dockerfile.debian" -t pc-inst-deb "$W" >/dev/null 2>&1 \
        && DEB_IMG=built || DEB_IMG=failed
    docker build -q -f "$W/Dockerfile.fedora" -t pc-inst-fed "$W" >/dev/null 2>&1 \
        && FED_IMG=built || FED_IMG=failed
    docker build -q -f "$W/Dockerfile.alpine" -t pc-inst-alp "$W" >/dev/null 2>&1 \
        && ALP_IMG=built || ALP_IMG=failed

    run_c() { # run_c <image> <name> <script-file> -- booted-systemd container
        local img=$1 name=$2 script=$3
        docker run -d --name "$name" --privileged --cgroupns=host \
            --tmpfs /run --tmpfs /run/lock --tmpfs /tmp --tmpfs /dev/shm \
            -v "$INSTALLER:/installer.sh:ro" "$img" >/dev/null 2>&1
        CONTAINERS+=("$name")
        # Wait for systemd to be up rather than guessing a sleep.
        for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
            docker exec "$name" test -d /run/systemd/system >/dev/null 2>&1 && break
            sleep 1
        done
        docker cp "$script" "$name:/inside.sh" >/dev/null 2>&1 || return 1
        docker exec "$name" bash /inside.sh 2>&1
    }

    # ---- debian: root, real .deb, booted systemd --------------------------
    cat > "$W/deb-root.sh" <<'EOS'
set -eu -o pipefail
sh /installer.sh > /out1.txt 2>&1 || { echo INSTALLER_FAILED; cat /out1.txt; exit 1; }
echo "R1=0"
echo "VER=$(/usr/bin/picoclaw version 2>&1 | tr -d '\r' | grep -m1 -E 'picoclaw [^ ]' | sed 's/^[[:space:]]*//')"
echo "DPKGVER=$(dpkg-query -W -f='${Version}' picoclaw)"
echo "ENABLED=$(systemctl is-enabled picoclaw 2>&1 || true)"
echo "UNIT=$([ -f /lib/systemd/system/picoclaw.service ] && echo present || echo absent)"
echo "LAUNCHERUNIT=$(systemctl is-enabled picoclaw-launcher 2>&1 || true)"
echo "USER=$([ -n "$(getent passwd picoclaw)" ] && echo created || echo missing)"
echo "ENVFILE=$([ -f /etc/picoclaw/env ] && echo seeded || echo absent)"
sh /installer.sh > /out2.txt 2>&1 || { echo RERUN_FAILED; cat /out2.txt; exit 1; }
echo "RERUN=$(grep -c 'already installed' /out2.txt || true)"
sh /installer.sh --force > /out3.txt 2>&1 || { echo FORCE_FAILED; cat /out3.txt; exit 1; }
echo "FORCE=$(grep -c 'Installed:' /out3.txt || true)"
echo "STILLENABLED=$(systemctl is-enabled picoclaw 2>&1 || true)"
EOS

    # ---- debian: unprivileged user, installer must reach sudo itself ------
    cat > "$W/deb-sudo.sh" <<'EOS'
set -eu -o pipefail
cd /home/tester
sudo -u tester -H sh /installer.sh > /tmp/u.txt 2>&1 \
    || { echo SUDO_FAIL; cat /tmp/u.txt; exit 1; }
echo "SUDO_RC=0"
echo "BIN=$(/usr/bin/picoclaw version 2>&1 | tr -d '\r' | grep -m1 -E 'picoclaw [^ ]' | sed 's/^[[:space:]]*//')"
EOS

    # ---- debian: forced tarball into a custom directory -------------------
    cat > "$W/deb-tar.sh" <<'EOS'
set -eu -o pipefail
sh /installer.sh --method tarball --install-dir /opt/pcbin > /t.txt 2>&1 \
    || { echo TAR_FAIL; cat /t.txt; exit 1; }
echo "TAR_RC=0"
echo "MODE=$(stat -c %a /opt/pcbin/picoclaw)"
echo "LAUNCHER=$([ -x /opt/pcbin/picoclaw-launcher ] && echo yes || echo no)"
echo "UNIT=$([ -e /lib/systemd/system/picoclaw.service ] && echo yes || echo no)"
echo "NOTINPATH=$(grep -c 'is not in PATH' /t.txt || true)"
EOS

    # ---- fedora: root, real .rpm ------------------------------------------
    cat > "$W/fed-root.sh" <<'EOS'
set -eu -o pipefail
sh /installer.sh > /o1.txt 2>&1 || { echo RPM_FAIL; cat /o1.txt; exit 1; }
echo "RPM_RC=0"
echo "VER=$(/usr/bin/picoclaw version 2>&1 | tr -d '\r' | grep -m1 -E 'picoclaw [^ ]' | sed 's/^[[:space:]]*//')"
echo "RPMQ=$(rpm -q --qf '%{VERSION}-%{RELEASE}' picoclaw)"
echo "UNIT=$([ -f /lib/systemd/system/picoclaw.service ] && echo present || echo absent)"
echo "ENABLED=$(systemctl is-enabled picoclaw 2>&1 || true)"
# rpm -Uvh refuses an identical version outright; the pre-check has to catch
# that first so a rerun is a clean no-op rather than a failed install.
sh /installer.sh > /o2.txt 2>&1 || { echo RERUN_FAIL; cat /o2.txt; exit 1; }
echo "RERUN=$(grep -c 'already installed' /o2.txt || true)"
EOS

    if [ "$DEB_IMG" = built ]; then
        echo "==> debian trixie, root, real .deb, booted systemd"
        out=$(run_c pc-inst-deb pc-inst-deb-root "$W/deb-root.sh")
        echo "$out" | sed 's/^/    /'
        chk "deb: installer exits 0" "yes" \
            "$(case $out in *INSTALLER_FAILED*) echo no;; *R1=0*) echo yes;; *) echo no;; esac)"
        chk "deb: installed binary reports the release" "yes" \
            "$(case $out in *VER=*"picoclaw ${VERSION#v} "*) echo yes;; *) echo no;; esac)"
        chk "deb: dpkg Version is nfpm's tilde form" "0.3.2~vicowa.1" \
            "$(printf '%s\n' "$out" | sed -n 's/^DPKGVER=//p')"
        chk "deb: gateway enabled on boot" "enabled" "$(printf '%s\n' "$out" | sed -n 's/^ENABLED=//p')"
        chk "deb: systemd unit installed" "present" "$(printf '%s\n' "$out" | sed -n 's/^UNIT=//p')"
        chk "deb: launcher unit left disabled" "disabled" "$(printf '%s\n' "$out" | sed -n 's/^LAUNCHERUNIT=//p')"
        chk "deb: picoclaw user created" "created" "$(printf '%s\n' "$out" | sed -n 's/^USER=//p')"
        chk "deb: /etc/picoclaw/env seeded" "seeded" "$(printf '%s\n' "$out" | sed -n 's/^ENVFILE=//p')"
        chk "deb: rerun is a no-op" "1" "$(printf '%s\n' "$out" | sed -n 's/^RERUN=//p')"
        chk "deb: --force reinstalls" "1" "$(printf '%s\n' "$out" | sed -n 's/^FORCE=//p')"
        chk "deb: still enabled after --force" "enabled" "$(printf '%s\n' "$out" | sed -n 's/^STILLENABLED=//p')"

        echo "==> debian trixie, unprivileged user reaching for sudo"
        out=$(run_c pc-inst-deb pc-inst-deb-sudo "$W/deb-sudo.sh")
        echo "$out" | sed 's/^/    /'
        chk "deb: sudo path exits 0" "0" "$(printf '%s\n' "$out" | sed -n 's/^SUDO_RC=//p')"
        chk "deb: sudo path installed a working binary" "yes" \
            "$(case $out in *BIN=*"picoclaw ${VERSION#v} "*) echo yes;; *) echo no;; esac)"

        echo "==> debian trixie, --method tarball into a custom dir"
        out=$(run_c pc-inst-deb pc-inst-deb-tar "$W/deb-tar.sh")
        echo "$out" | sed 's/^/    /'
        chk "tarball: custom --install-dir exits 0" "0" "$(printf '%s\n' "$out" | sed -n 's/^TAR_RC=//p')"
        chk "tarball: mode 0755" "755" "$(printf '%s\n' "$out" | sed -n 's/^MODE=//p')"
        chk "tarball: launcher installed" "yes" "$(printf '%s\n' "$out" | sed -n 's/^LAUNCHER=//p')"
        chk "tarball: installs no systemd unit (as documented)" "no" "$(printf '%s\n' "$out" | sed -n 's/^UNIT=//p')"
    else
        skip "debian container" "image build failed"
    fi

    if [ "$FED_IMG" = built ]; then
        echo "==> fedora, root, real .rpm, booted systemd"
        out=$(run_c pc-inst-fed pc-inst-fed-root "$W/fed-root.sh")
        echo "$out" | sed 's/^/    /'
        chk "rpm: installer exits 0" "yes" \
            "$(case $out in *RPM_FAIL*) echo no;; *RPM_RC=0*) echo yes;; *) echo no;; esac)"
        chk "rpm: installed binary reports the release" "yes" \
            "$(case $out in *VER=*"picoclaw ${VERSION#v} "*) echo yes;; *) echo no;; esac)"
        chk "rpm: package is queryable" "yes" "$(case $out in *RPMQ=0.3.2*) echo yes;; *) echo no;; esac)"
        chk "rpm: systemd unit installed" "present" "$(printf '%s\n' "$out" | sed -n 's/^UNIT=//p')"
        chk "rpm: gateway enabled on boot" "enabled" "$(printf '%s\n' "$out" | sed -n 's/^ENABLED=//p')"
        chk "rpm: rerun is a clean no-op" "1" "$(printf '%s\n' "$out" | sed -n 's/^RERUN=//p')"
    else
        skip "fedora container" "image build failed"
    fi

    # ---- alpine: busybox wget, musl, no package manager -------------------
    # Written to a file rather than an inline `sh -c '...'`: the probe needs
    # single quotes of its own, and nesting them inside the docker argument
    # truncates the script at the first one.
    cat > "$W/alp.sh" <<'EOS'
set -eu
if command -v curl >/dev/null 2>&1; then echo CURL_PRESENT; exit 1; fi
if command -v dpkg >/dev/null 2>&1; then echo HAS_DPKG; exit 1; fi
sh /installer.sh > /o.txt 2>&1 || { echo ALP_FAIL; cat /o.txt; exit 1; }
echo "ALP_RC=0"
echo "VER=$(picoclaw version 2>&1 | tr -d '\r' | grep -m1 -E 'picoclaw [^ ]' | sed 's/^[[:space:]]*//')"
echo "METHOD=$(grep -c 'Method   : tarball' /o.txt || true)"
echo "WGET=$(grep -c 'Verified : SHA-256' /o.txt || true)"
EOS

    if [ "$ALP_IMG" = built ]; then
        # Alpine has no dpkg, no rpm, no curl and no bash: it exercises the
        # busybox wget downloader, the tarball fallback and POSIX sh parsing.
        echo "==> alpine: busybox wget, no package manager, musl"
        out=$(docker run --rm -v "$INSTALLER:/installer.sh:ro" -v "$W/alp.sh:/alp.sh:ro" \
            pc-inst-alp sh /alp.sh 2>&1)
        echo "$out" | sed 's/^/    /'
        chk "alpine: busybox wget download works" "yes" \
            "$(case $out in *ALP_FAIL*|*CURL_PRESENT*) echo no;; *ALP_RC=0*) echo yes;; *) echo no;; esac)"
        chk "alpine: falls back to tarball" "1" "$(printf '%s\n' "$out" | sed -n 's/^METHOD=//p')"
        chk "alpine: checksum verified without curl" "1" "$(printf '%s\n' "$out" | sed -n 's/^WGET=//p')"
        chk "alpine: binary runs on musl" "yes" \
            "$(case $out in *VER=*"picoclaw ${VERSION#v} "*) echo yes;; *) echo no;; esac)"
    else
        skip "alpine container" "image build failed"
    fi
fi


# ------------------------------------------------------------------ summary --
echo
echo "installer-suite: $PASS passed, $FAIL failed, $SKIP skipped"
[ "$FAIL" -eq 0 ] || exit 1
