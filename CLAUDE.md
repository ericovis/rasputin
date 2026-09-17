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
go run ./cmd/rasputin sync -plan -json   # read-only: what sync would do, as data
go run ./cmd/rasputin manual             # the embedded manual (cmd/rasputin/MANUAL.md)
make install-man MANDIR=/opt/homebrew/share/man/man1   # `man rasputin` on this Mac
```

`make install-man` defaults to `/usr/local/share/man/man1`, which is
root-owned here; Homebrew's `man1` is on the man path and writable, and that
is where the page is installed on this machine. It is a snapshot — reinstall
after editing `MANUAL.md`.

Every command takes `-json`: stdout becomes newline-delimited JSON objects,
last one `{"type":"result","command":…,"ok":…}`. The contract is in
`cmd/rasputin/output.go` (envelope) and `cmd/rasputin/json.go` (views), and
is documented field by field in `cmd/rasputin/MANUAL.md`. **A new command,
flag or result field is not done until the manual says so** — the manual is
what an agent driving the tool reads, and `TestManual` checks every command
has a section. In JSON mode nothing but JSON may reach stdout: commands print
through `output.printf` (dropped in JSON mode) and `output.logf` (becomes a
log object), never `fmt.Print`. `manual -man` renders the same Markdown as
roff through go-md2man (pure Go, the only dependency added for it); the
Markdown stays the single source, so there is no `.1` file to keep in step.

`sync` is idempotent and is now the normal way to drive the cluster; the single
commands stay as the escape hatch. `reset` (and `sync -reset`) is the fast
loop: the golden rootfs is the read-only lower layer of an overlay and p3 is
the writable upper layer, so putting a node back to the image is emptying p3
and rebooting, not reflashing. It refuses to start when any node is
unreachable, and every event it emits is also appended to `out/sync.log`.
`internal/events` is the contract between orchestration and UI — orchestration
emits `events.Event`, never draws. `internal/tui` is the **only** package
allowed to import bubbletea/lipgloss; nothing else may grow a TTY dependency.

`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` must always pass.
Linux-only syscalls live behind build tags with `!linux` stubs so the module
still vets and tests on the Mac.

## Ground rules

- **The remote is `github.com/ericovis/rasputin`. Commit freely; push only
  when the owner asks.** `out/` and `cache/` are gitignored and must stay
  uncommitted, and so must anything that could hold a secret (`.gitignore`
  lists the patterns). The `plan/` docs and the `legacy/` shell prototype
  were removed 2026-08-29; their history (including every hardware bug and
  its cause) is in git.
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
  `prepare.Options.RemoveImage`. Nothing reads the raw image any more — day 0
  is `write-card -image vanilla`, which decodes the `.zst` — but `prepare`
  still keeps it, because it is the one artifact that survives a botched
  compression and 2.9 GB has run `/` out of space mid-capture before.
- **Never gate on the golden's compressed size.** Every capture carries
  whatever bytes are on the card, so the compressed size tracks the card's
  history as much as the build — a 4 GiB golden once measured *larger* than an
  8 GiB one whose tail happened to be trimmed to zeros. `seal` now zeroes the
  free space (bounded: the builder's rootfs is capped at
  `image.rootfs_size_gb`, and it skips with a log line if `df` says the
  filesystem is more than 10% over the cap), so sizes drop after the next bake
  — and are still not a gate. Gate on `card_used_bytes`, the decode
  verification (`verifyImage` requires the decoded length to equal the
  partition table's `UsedBytes`) and a real clone.
- **A flashed node reboots twice.** `rasputin-identity` appends p3 and
  reboots on the clone's first boot, before sshd ever starts, so `flash` and
  `bake` just see a boot that takes about a minute longer. Nothing may make
  that first boot reachable before the reboot: a service coming up on the bare
  rootfs would write into what is about to become the read-only layer.
- **A reset must carry the node's host keys over.** `seal` strips
  `/etc/ssh/ssh_host_*` and `/etc/machine-id` from the golden, so
  `rasputin-identity` mints them on the clone's first boot — into the *upper*
  layer, since the rootfs is read-only by then. Emptying the layer wholesale
  would hand the node a new SSH identity on every reset and break the pinned
  key, `known_hosts` and the DHCP lease. `agent.WipeUpper` copies
  `IdentityGlobs` out and back; the regression test is
  `TestWipeUpperKeepsTheNodesIdentity`.
- **The overlay is a kernel module the agent loads itself.** The Pi kernel
  builds overlayfs as `overlay.ko.xz` with no in-kernel decompressor, so the
  initramfs reads it out of the rootfs (the upper layer first — an apt kernel
  upgrade puts new modules there), unpacks it with `github.com/ulikunitz/xz`
  and `init_module(2)`s it. Any failure on that path falls back to booting p2
  read-write with no overlay, loudly, because a node that boots is fixable and
  one that does not is not: `status` then shows `overlay: no`, `reset` refuses
  the node, and whatever it writes goes into the golden bytes themselves.
- **A sync run from another machine leaves this machine's pins stale.**
  `out/state.json` pins each node's host key, and only the machine that
  flashes drops the pin; after somebody else reflashes the cluster, every
  connection here fails as a changed host key and `sync` refuses to plan.
  `rasputin forget <node|all>` drops the pins, `sync -trust-new-keys` re-pins
  as it goes; the plan's refusal says so because `sshx.ErrHostKeyChanged`
  survives the error chain through `nodes.Resolver.Connect`.
- **Never trust a hostname.** This cluster had two nodes answering to
  `rasputin002`. Every connection verifies `/sys/class/net/eth0/address`
  against the config before acting. Do not weaken that.
- **A raw disk node takes whole sectors only.** On macOS a write to
  `/dev/rdiskN` that is not a multiple of 512 fails with EINVAL, and the zstd
  decoder hands out whatever the frame holds. `internal/card`'s `aligned`
  buffers the stream into sector multiples; the agent needs none of that
  because `/dev/mmcblk0` is a block device. `Sync` is **not** the end of the
  stream — the shared pipeline calls it every 256 MiB — so it flushes whole
  sectors only and the tail waits for `finish()`. Regression tests:
  `TestAlignedSyncKeepsThePartialSector` and `TestWriteDecodesTheWholeImage`,
  whose image is not a whole number of write blocks.
- **`write-card` must never offer an internal disk.** The picker erases what
  is chosen from it, on the owner's own Mac. `card.device` drops anything
  `Internal`, `disk0` by name, not a whole disk, or virtual, and `diskutil` is
  asked for `external physical` in the first place. Regression test:
  `TestDeviceNeverOffersThisMachinesOwnDisk`. Do not weaken that, and do not
  add a default `-device`.

## The cluster

Four Pi 3 Model B, wired, DHCP on 192.168.0.0/24, 32 GB cards, Raspberry Pi
OS Trixie (arm64). All four now run the golden image (build
`20260908T231900Z-cf9898`) as user **`berry`** — `ericovis` no longer exists
on them. MACs are in `rasputin.yaml`; that is the identity that matters, not
the name or the IP. **`rasputin.yaml` is gitignored and exists only on this
Mac** (history was rewritten 2026-09-08 to remove it and the real MACs; the
tests use `internal/config/testdata/cluster.yaml`, which is made up). `rasputin001` is the builder.

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
