#!/bin/sh
# Create the picoclaw service account and its state directories.
#
# Runs as a package preinstall script (deb and rpm) and is also sourced by
# install.sh, so it has to be idempotent: it runs again on every upgrade,
# and must never touch existing data.
#
# Never deletes anything on removal. /var/lib/picoclaw holds the user's
# config, workspace, skills and session memory; retiring an account that some
# other package might have come to depend on is not worth the risk either.
set -eu

PICOCLAW_HOME_DIR=/var/lib/picoclaw
CONF_DIR=/etc/picoclaw

# nologin lives in different places across distros; /bin/false is the fallback.
if [ -x /usr/sbin/nologin ]; then
    SHELL_NOLOGIN=/usr/sbin/nologin
elif [ -x /sbin/nologin ]; then
    SHELL_NOLOGIN=/sbin/nologin
else
    SHELL_NOLOGIN=/bin/false
fi

getent group picoclaw >/dev/null 2>&1 || groupadd --system picoclaw

getent passwd picoclaw >/dev/null 2>&1 ||
    useradd --system \
        --gid picoclaw \
        --home-dir "$PICOCLAW_HOME_DIR" \
        --shell "$SHELL_NOLOGIN" \
        --comment "PicoClaw daemon" \
        picoclaw

# 0750: the daemon owns its state, nothing else on the box reads it.
#
# Only when the directory is missing. `install -d -o … -m …` re-asserts owner
# and mode every time it runs, so on an upgrade it would silently revert an
# operator's own chmod/chown on these two directories — the header promises this
# script never touches existing data.
[ -d "$PICOCLAW_HOME_DIR" ] ||
    install -d -o picoclaw -g picoclaw -m 0750 "$PICOCLAW_HOME_DIR"
# Secrets land here (env file). Root writes, the group reads.
[ -d "$CONF_DIR" ] ||
    install -d -o root -g picoclaw -m 0750 "$CONF_DIR"
