# CLAUDE.md

Go CLI that reflashes a 4-node Raspberry Pi 3 cluster over the network.
Read `README.md` for how it works. This file is the stuff that is *not*
obvious from the code, and that cost real time to learn.

## Commands

```sh
make build        # CLI + out/recovery.gz
make test         # go test ./... and a linux/arm64 cross-build
go test ./...     # must pass on darwin — no hardware needed

go run ./cmd/rasputin init  # writes rasputin.yaml; needs -force to overwrite
go run ./cmd/rasputin sync    # probe, plan, confirm, then only the stale steps
go run ./cmd/rasputin sync -plain -yes   # no TUI, no prompt: CI and repro runs
```

`sync` is idempotent and is now the normal way to drive the cluster; the single
commands stay as the escape hatch. It refuses to start when any node is
unreachable, and every event it emits is also appended to `out/sync.log`.
`internal/events` is the contract between orchestration and UI — orchestration
emits `events.Event`, never draws. `internal/tui` is the **only** package
allowed to import bubbletea/lipgloss; nothing else may grow a TTY dependency.

`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` must always pass.
Linux-only syscalls live behind build tags with `!linux` stubs so the module
still vets and tests on the Mac.

## Ground rules

- **No git remote. Never push.** Commit freely; `out/` and `cache/` are
  gitignored and must stay uncommitted. The `plan/` docs and the `legacy/`
  shell prototype were removed 2026-08-29; their history (including every
  hardware bug and its cause) is in git.
- Hardware operations are destructive (a flash is ~6 min, a bake ~16 min,
  both measured 2026-08-29). Confirm with the owner before wiping a node.
- `bake` needs ~5 GB free on `/`. Disk has run out mid-capture before;
  `out/vanilla-custom.img` (2.9 GB) is a regenerable intermediate and is the
  first thing to delete — delete it after every `prepare`.
- Docker is not used today, but the old "MUST NOT be used" rule is revised:
  the owner has approved a future opt-in `bake --strategy=local` that
  provisions the golden in an arm64 container. It must stay an explicit
  flag (never auto-selected — an on-Pi bake proves the image boots, a local
  one does not), record its strategy in `golden.json`, and reuse the same
  rendered provision/seal scripts rather than reimplementing them.

## Traps that have already bitten

Each of these has a regression test. If you touch the area, run it.

- **Clear a pinned SSH host key only *after* the node is down.** `waitGone`
  polls with real SSH connections, and one of those re-pins the key that is
  about to be destroyed. See `RebootOptions.ReplacesSystem`.
- **A capture must delete its own flag file *before* reading the card.** The
  capture streams the boot partition too, so a flag left on disk gets baked
  into the golden image and every clone then tries to capture itself.
- **`userconfig.service` must stay masked.** Raspberry Pi OS's interactive
  first-boot dialog is a oneshot on `/dev/tty8` with no timeout; on a
  headless node it blocks `multi-user.target` *forever*, so systemd never
  reaches `running` and nothing ordered after that target ever starts.
- **`seal` must not delete `/var/lib/rasputin/provisioned`.** Removing it
  makes every clone re-run apt on first boot, needing the internet to boot.
- **go-diskfs over-reads FAT files** to the end of the last cluster (stock
  `config.txt` is 1272 B but reads back as 1536 B). Bound every read by
  `Stat().Size()` — `bootfs.ReadFile` does.
- **A Pi has no RTC.** Before NTP syncs, `date` on a node returns the
  image's fake-hwclock date (months stale). Never timestamp anything from
  the node; template build-host time in instead.
- **`sudo -k -S`: the `-k` is load-bearing.** It forces sudo to consume
  exactly one stdin line. Without it a cached credential makes sudo skip
  that read, and `Push` would write the password into the file.
- **The rootfs cap is consumed at `prepare` time, not `bake` time.**
  `rasputin.yaml` `image.rootfs_size_gb` is templated into `firstrun.sh` and
  `seal.sh` inside `out/vanilla-custom.img.zst`. Editing the yaml and baking
  without re-running `prepare` silently bakes at the *old* cap. Always: edit
  yaml → `prepare` → `rm out/vanilla-custom.img` → `bake`. `sync` does that
  sequence for you and is the reason it exists.
- **`prepare`'s idempotency key does not cover Go code.**
  `prepare.Fingerprint` hashes `image.source_url` and the *rendered provision
  files* plus `nodes.conf`, so it moves when `rasputin.yaml` or an
  `internal/provision` template moves, and stays put for a comment edit. A change to
  `internal/agent` or `internal/initramfs` — i.e. to `recovery.gz` — is
  **not** in it, so `sync` will happily skip prepare and bake a golden carrying
  the old agent. After touching either package: `sync -force-prepare`, or a
  plain `prepare`.
- **`sync` deletes `out/vanilla-custom.img`, plain `prepare` keeps it.**
  `prepare.Options.RemoveImage`. Day 0 `dd`s that raw image to a card, so the
  `prepare` command must not remove it; `sync` never needs it after compressing
  and 2.9 GB has run `/` out of space mid-capture before.
- **Never gate on the golden's compressed size.** `seal` deliberately does not
  zero free space, so every capture carries whatever stale bytes are on the
  card and the compressed size tracks the card's history, not the build — a
  4 GiB golden measured *larger* than an 8 GiB one whose tail happened to be
  trimmed to zeros. Gate on `card_used_bytes`, the decode verification
  (`verifyImage` requires the decoded length to equal the partition table's
  `UsedBytes`) and a real clone.
- **Never trust a hostname.** This cluster had two nodes answering to
  `rasputin002`. Every connection verifies `/sys/class/net/eth0/address`
  against the config before acting. Do not weaken that.

## The cluster

Four Pi 3 Model B, wired, DHCP on 192.168.0.0/24, 32 GB cards, Raspberry Pi
OS Trixie (arm64). All four now run the golden image (build
`20260829T205033Z-307368`) as user **`berry`** — `ericovis` no longer exists
on them. MACs are in `rasputin.yaml`; that is the identity that matters, not
the name or the IP. `rasputin001` is the builder.

- The owner's SSH key is **passphrase protected** and used through the macOS
  ssh-agent. `sshx` tries the agent first, then the key file.
- The wire tops out around 10 MB/s and a flashing node consumes ~4.3 MB/s,
  so up to two concurrent flashes run at solo speed; three or four share the
  link and each slows ~20–25%. Physics, not a regression.
- All four nodes have passwordless sudo. `ssh.sudo: password` in the YAML is
  the fallback for nodes that do not; the password comes from
  `$RASPUTIN_SUDO_PASSWORD` or a prompt, **never** from the config file.
- A flashed node regenerates its SSH host keys, so `ssh` will complain about
  `REMOTE HOST IDENTIFICATION HAS CHANGED` until `known_hosts` is cleared —
  `flash` and `bake` now do that for you automatically.

## Testing without hardware

The whole suite runs on the Mac: agent pipelines against `httptest`, the SSH
client against an in-process SSH server, image contents by re-reading the FAT
partition with go-diskfs. Nothing mounts anything or needs root. Keep it that
way — `internal/prepare`'s `TestImageContents` is the end-to-end gate that
catches a broken image before a node does.
