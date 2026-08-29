# rasputin

Reflash a headless Raspberry Pi cluster over the network, with one command
and no physical access.

```
rasputin flash all
```

Every node is a byte-identical clone of one golden image, and every node can
be rebuilt from scratch remotely — including the node the golden image was
baked on. There is no Docker, no loop mount, no `sudo` on the build host, and
nothing to plug in after a node has been adopted once.

## How it works

Four ideas do all the work.

**The recovery agent is the initramfs.** `cmd/agent` is a single static
aarch64 binary that *is* `/init`. It is packed into `recovery.gz` (a gzipped
newc cpio holding just `/init` and a `/dev/console` node) and loaded on every
boot via `initramfs recovery.gz followkernel` in `config.txt`. Normally it
mounts the real root and `switch_root`s into it, adding about two seconds to
boot. When a flag file is present on the boot partition it does something
else instead — and because it lives entirely in RAM, it can overwrite the
card underneath itself without harming itself.

**Flag files are the trigger protocol.** A file on the FAT boot partition
selects the mode, and its first line, if non-empty, overrides the image URL:

| flag file | what the agent does |
|---|---|
| *(none)* | normal boot |
| `reflash` | stream an image and write the whole card, then reboot |
| `reflash-dryrun` | run the same pipeline into a discard writer, leave a report, reboot |
| `capture` | stream the used part of the card back to the CLI (this is how golden images are made) |

`reflash-dryrun` beats `capture` beats `reflash`, so a rehearsal can never
turn into a wipe. This is a plain protocol, not an API: a flag file plus a
reboot is a complete trigger, which is why the escape hatch below works.

**Customisation touches only the boot partition.** The CLI writes to the
FAT32 partition with pure-Go `go-diskfs` — never the ext4 root. Everything
that has to change inside the root filesystem is done *by the Pi itself*, on
first boot, by a generated `firstrun.sh` launched through the official
Raspberry Pi Imager mechanism (`systemd.run=` in `cmdline.txt`). That is what
keeps image preparation to an unprivileged, dependency-free program.

**Golden images are baked on a Pi.** There is no cross-compilation of a root
filesystem and no emulation. One node (`builder:` in the config) is reflashed
with a prepared stock image, provisions itself, is stripped of everything
unique, and then streams its own card back over HTTP. Every later flash of
any node is a clone of that. Identity is reapplied on each boot by a baked-in
service that maps the node's ethernet MAC to a hostname via `nodes.conf`;
SSH host keys and `machine-id` regenerate themselves.

## Quickstart

```sh
$EDITOR rasputin.yaml        # nodes, MACs, users, packages, builder
make build                   # CLI + out/recovery.gz
go run ./cmd/rasputin prepare      # stock image + customised boot partition
go run ./cmd/rasputin adopt all    # install recovery on live nodes (no wipe)
go run ./cmd/rasputin bake         # build the golden image (WIPES the builder)
go run ./cmd/rasputin flash all    # clone it everywhere, in parallel
go run ./cmd/rasputin status       # read-only health table
```

Requirements: Go 1.27 on the build host, an SSH key that reaches the nodes
(loaded in your agent if it has a passphrase), and **passwordless sudo for
your SSH user on every node** — the CLI writes to the boot partition and
reboots. Grant it once per node with:

```sh
ssh <user>@<node> "echo '<user> ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/<user>"
```

## Commands

| command | what it does |
|---|---|
| `prepare` | build `recovery.gz`, fetch and customise the stock image |
| `adopt <node\|all>` | install the recovery mechanism on a live node, over SSH. Does not flash |
| `dryrun <node\|all>` | rehearse the whole download-and-decode pipeline. Never opens the card for writing |
| `bake` | reflash the builder, let it provision, seal it, capture it as `out/golden.img.zst` |
| `flash <node...\|all>` | clone the golden image onto nodes, in parallel, and verify each one |
| `status` | node, address, SSH user, hostname, build id, provisioned, uptime. Read-only |
| `serve` | run the HTTP server alone, for hand-triggering a flash |

## Day 0: a virgin SD card

`adopt` only works on a node that already runs an OS and answers SSH. A blank
card needs one physical write, after which the node is remotely manageable
forever:

```sh
go run ./cmd/rasputin prepare        # writes out/vanilla-custom.img

diskutil list                        # find the card, e.g. /dev/disk4
diskutil unmountDisk /dev/disk4
sudo dd if=out/vanilla-custom.img of=/dev/rdisk4 bs=4m status=progress
sync
diskutil eject /dev/disk4
```

Use `/dev/rdiskN` (raw), not `/dev/diskN` — it is roughly an order of
magnitude faster. Boot the Pi; `firstrun.sh` creates the user, grows the root
filesystem, and installs the identity service. From then on the node answers
`adopt`, `bake` and `flash` over the network.

## Day 2: changing the fleet

```sh
$EDITOR rasputin.yaml          # e.g. add a package
go run ./cmd/rasputin prepare
go run ./cmd/rasputin bake     # wipes and rebuilds the builder
go run ./cmd/rasputin flash all
```

## Escape hatch: trigger a flash by hand

The flag-file protocol needs no CLI. With `rasputin serve` running:

```sh
ssh <node> 'echo http://<mac-ip>:8080/i/golden.img.zst | sudo tee /boot/firmware/reflash && sudo reboot'
```

`rasputin serve` prints the exact line to paste.

## Troubleshooting

**A node is stuck in the recovery agent.** That is the designed failure mode,
not a brick. The agent retries forever with backoff and **never opens the
card for writing until an HTTP probe of the image URL has succeeded** — so a
node that cannot reach the server still has its original system intact.
Check that the build host is serving (`rasputin serve`) and on the same LAN,
then power-cycle. Its progress is on the console and in `dmesg` (every line
is prefixed `rasputin:`).

**A dryrun failed.** The node writes `reflash-dryrun.log` to its boot
partition before rebooting; `rasputin dryrun` prints it. It records the URL,
the MAC, and either `result: OK <bytes> in <time>` or the failure.

**An adopt went wrong and the node will not boot.** Pull the card and either
delete the `initramfs recovery.gz followkernel` line from `config.txt`, or
restore the backup `adopt` always makes first:

```sh
cp /Volumes/bootfs/config.txt.pre-rasputin /Volumes/bootfs/config.txt
```

**`REMOTE HOST IDENTIFICATION HAS CHANGED` after a flash.** Expected — see
below. `ssh-keygen -R <node>` clears the stale entry.

## Security trade-offs

These are deliberate, and worth knowing before you point this at a network
you do not own.

- **Host keys change on every flash.** A cloned node must not share a
  fingerprint with its siblings, so the golden image ships with none and each
  node generates its own on first boot. The CLI therefore cannot use
  `known_hosts`: it pins each node's key in `out/state.json` on first sight
  and drops the pin itself whenever *it* is the one replacing the system. A
  key that changes at any other time is reported as an error. Your own `ssh`
  client will need `ssh-keygen -R` after a flash.
- **Node identity is verified by MAC, not by name.** Every connection reads
  `/sys/class/net/eth0/address` and refuses to act on a machine whose MAC
  does not match the config. This is not paranoia: this cluster has had two
  nodes answering to one hostname, and flashing the wrong Pi has no undo.
- **HTTP, not HTTPS, and no authentication.** Images are served in the clear
  to whoever asks, and any host that can reach the server can fetch one.
  Intended for a trusted LAN only. The image contains your `authorized_keys`
  but no private key.
- **Passwordless sudo is required** on every managed node.
- `out/state.json` is mode 0600 and records which accounts exist on the
  cluster.

## Layout

```
cmd/rasputin      the CLI
cmd/agent         the recovery agent — becomes /init inside recovery.gz
internal/agent    boot, flag selection, switch_root, reflash/dryrun/capture
internal/bootfs   FAT32 boot-partition editing and config.txt/cmdline.txt patches
internal/cluster  orchestration: adopt, dryrun, bake, flash, status
internal/cpio     newc writer and reader
internal/initramfs  cross-compiles the agent and packs it
internal/nodes    resolution (name/MAC/IP/all), MAC verification, preflight
internal/prepare  the prepare pipeline
internal/provision  firstrun/identity/provision/seal templates
internal/server   HTTP server: image serving, capture receipt, progress
internal/sshx     SSH client with user fallback and host-key pinning
internal/vanilla  stock image download, xz decode, cache
legacy/           the shell prototype this replaced, kept as a reference
plan/             the execution plan, progress log and blockers
```

## Development

```sh
make build   # CLI + recovery.gz
make test    # go test ./... and a linux/arm64 cross-build
make clean
```

Tests run on macOS. Linux-only syscalls are behind build tags so the module
still vets and tests on the build host, and `GOOS=linux GOARCH=arm64 go build
./...` must always pass. Nothing in the test suite needs hardware: the agent
pipelines run against `httptest`, the SSH client against an in-process SSH
server, and image contents are verified by re-reading the FAT partition with
`go-diskfs` rather than by mounting anything.
