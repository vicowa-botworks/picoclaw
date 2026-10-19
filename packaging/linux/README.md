# Linux service bundle

systemd units, secrets template, and packaging glue to run the PicoClaw gateway
on Linux and bring it back up at boot. The units are architecture-neutral and
ship in every `.deb`/`.rpm`; Raspberry Pi OS and other 64-bit ARM
Debian/Ubuntu hosts are what they were built and tested against.

Nothing here is required to build PicoClaw; it is the deployment half.

```
packaging/linux/
├── systemd/
│   ├── picoclaw.service           # the gateway — enabled at boot
│   └── picoclaw-launcher.service  # the web console — installed, NOT enabled
├── env.example                    # -> /etc/picoclaw/env  (API keys live here)
├── scripts/
│   ├── preinstall.sh              # creates the picoclaw account + state dirs
│   ├── postinstall.sh             # enables the unit, restarts on upgrade
│   ├── preremove.sh               # stops and disables on removal only
│   └── postremove.sh              # daemon-reload; keeps your data
├── install.sh                     # installer for the raw binaries / tar.gz
└── README.md
```

## Install

**With the .deb** (this is all it takes — the account, the units, and boot
enablement are handled by the package):

```sh
sudo dpkg -i picoclaw_aarch64.deb
```

**With the tar.gz / loose binaries** — from a directory holding `picoclaw`,
`picoclaw-launcher`, and this `packaging/linux/` tree:

```sh
sudo ./install.sh                         # add --with-launcher to enable the console too
```

`install.sh` also works inside a chroot while you are building an SD-card
image; with no running systemd it lays down the boot symlink by hand and tells
you to `systemctl enable` on first boot.

## First boot

The gateway needs a configured default model, so the unit is gated on the config
file existing. On a fresh install `systemctl start picoclaw` therefore does nothing
visible — that is intended, not a failure. Configure it once:

```sh
sudo -u picoclaw PICOCLAW_HOME=/var/lib/picoclaw picoclaw onboard
sudoedit /etc/picoclaw/env          # API keys go here, NOT in config.json
sudo systemctl start picoclaw
systemctl status picoclaw --no-pager
```

`/etc/picoclaw/env` is installed `0640 root:picoclaw` from `env.example`.
Provider keys are read from the environment; `config.json` rejects an `api_key`
field outright, so the env file is the only way in.

Alternatively skip the CLI and use the console (below) — running the launcher
also creates `config.json`, which satisfies the gateway's condition.

## Day to day

```sh
sudo systemctl status picoclaw
sudo systemctl restart picoclaw
sudo journalctl -u picoclaw -f
```

The gateway also writes structured logs to `$PICOCLAW_HOME/logs/`
(`pkg/gateway/gateway.go` `logPath`), plus a `*_panic.log` for crashes.

| Path | What |
| --- | --- |
| `/var/lib/picoclaw` | `PICOCLAW_HOME`: `config.json`, workspace, skills, sessions, `.picoclaw.pid` |
| `/etc/picoclaw/env` | API keys and channel tokens |
| `/usr/bin/picoclaw` | gateway |
| `/usr/bin/picoclaw-launcher` | web console |

Both listen on loopback only by default: gateway `127.0.0.1:18790`, console
`127.0.0.1:18800`.

## The web console

`picoclaw-launcher.service` ships **disabled**. It is a configuration tool, not
something to leave running 24/7 on a 1GB board, and it is the only piece that
would face the network.

```sh
sudo systemctl start picoclaw-launcher
ssh -L 18800:localhost:18800 <user>@<host>      # from your laptop
# open http://localhost:18800  ->  /launcher-setup sets the dashboard password
```

It deliberately does not pass `-public`. If you decide you want the console on
the LAN, set the dashboard password first, then add a drop-in rather than
editing the shipped unit:

```sh
sudo systemctl edit picoclaw-launcher.service
#   [Service]
#   ExecStart=
#   ExecStart=/usr/bin/picoclaw-launcher -console -no-browser -port 18800 -public
```

## Hardening

Both units run as the `picoclaw` system user with `ProtectSystem=strict`, so the
whole filesystem is read-only except `/var/lib/picoclaw`. That is a deliberate
choice: the agent's `exec` tool runs real commands as that user, and this bounds
the blast radius on a machine that probably does other things too.

Also on: `PrivateDevices`, `PrivateTmp`, `ProtectHome`,
`ProtectKernel{Tunables,Modules,Logs}`, `ProtectControlGroups`,
`RestrictNamespaces`, `RestrictRealtime`, `RestrictSUIDSGID`, `LockPersonality`,
`NoNewPrivileges`, `SystemCallArchitectures=native`, and an empty
`CapabilityBoundingSet`.

One consequence worth knowing before it surprises you: `NoNewPrivileges` plus
`RestrictSUIDSGID` mean **setuid helpers do not work** inside the service —
`sudo`, `ping`, `mount`, and anything else that escalates through a setuid bit
fails, however the `picoclaw` account is configured. That is intended, and it is
the first thing you hit if you ask the agent's `exec` tool to do something that
needs privilege. Run the privileged step outside the service, or relax both
directives in a drop-in if you really mean it.

`RestrictAddressFamilies` keeps `AF_NETLINK` for a reason: `pkg/netbind` calls
`net.InterfaceAddrs()`, which Go implements over netlink. Drop it and gateway
binding breaks.

To relax something, always use a drop-in — never edit the packaged file, or your
change is lost on the next upgrade:

```sh
sudo systemctl edit picoclaw.service
```

## Upgrades and removal

Upgrading restarts the gateway **only if it was already running**
(`systemctl try-restart`), so a fresh install never starts a daemon with no
config, and a daemon you stopped on purpose stays stopped.

Removal stops and disables both units but keeps `/var/lib/picoclaw`,
`/etc/picoclaw`, and the `picoclaw` account. Deleting a uid that other software
may have come to own is not reversible. To finish it by hand:

```sh
sudo rm -rf /var/lib/picoclaw /etc/picoclaw
sudo deluser --remove-home picoclaw
```

## How the package is produced

`.goreleaser.yaml` builds 5 OSes × 7 arches × 2 binaries. Cutting one
`.deb` out of that means narrowing the target list, which is what
`goreleaser-snapshot.yaml` does — generated by reading the real config and overriding
only `goos`/`goarch` and `dist`, so build tags, ldflags, `nfpms` metadata and
`archives` are inherited rather than retyped. It is a scratch artifact, not
checked in; the files in this directory are the part that belongs in the repo.

Two constraints worth knowing before you touch that config:

- goreleaser's `nfpms` schema exposes only `contents` and `scripts` — there is
  no `systemd:` or `presets:` key like nfpm itself has. The units ship as
  `contents` and the maintainer scripts do the enable/disable.
- A snapshot version must sort **below** any official release. dpkg compares
  strings, so a label like `pi4-snapshot` is both invalid syntax and, if it
  parsed, would outrank `0.2.9` and block future upgrades. The default
  `0.0.0~SNAPSHOT-<commit>` is correct: `~` sorts low and the commit is kept.

## Enabling a unit from a package: the trap

`postinstall.sh` uses `systemctl enable`, not `deb-systemd-helper enable`.

On Debian trixie, `deb-systemd-helper enable` alone writes its bookkeeping under
`/var/lib/systemd/deb-systemd-helper-enabled/` but does **not** create the
`.wants` symlink for a unit it has never seen enabled before. A package relying
on it installs as `disabled` — for this bundle that means silently never starting
at boot, and only on a system where systemd is actually running, so it passes
CI-style chroot tests and fails on the real device. `systemctl enable` creates
the symlink whether or not systemd is PID 1, so it is correct in a chroot and on
a booted system alike.

## Tests

```sh
sh packaging/linux/tests/scriptlets.sh        # seconds, no privileges needed
packaging/linux/tests/container-suite.sh      # minutes, needs docker + nfpm
```

`scriptlets.sh` is what CI runs (`.github/workflows/packaging.yml`). It drives
the scriptlets against a stub `systemctl` and asserts what they would have done
for every argument they can be handed — including the one that used to be
missed: RPM passes `%preun` a **number** (`1` = upgrade, `0` = erase) where dpkg
passes a word.

`container-suite.sh` is the real thing. It builds three RPM versions from the
scripts in this tree, boots systemd in a privileged container, and walks
install → operator edits → `rpm -Uvh` → masked `rpm -Uvh` → erase, asserting
that the gateway is still `enabled` and `active` afterwards, that the secrets
file keeps its `0640 root:picoclaw` mode and its contents, that an operator's
`chmod` on `/var/lib/picoclaw` survives the upgrade, and that a masked unit
stays masked. Run it before a release.

It has to run natively on x86_64. Under qemu-aarch64 every unit here dies with
`status=226/NAMESPACE` — namespace directives are not emulated — and sandbox
behaviour is arch-independent anyway, so x86_64 is the honest host.
