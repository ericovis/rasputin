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
(loaded in your agent if it has a passphrase), and a way to run `sudo` on
them — the CLI writes to the boot partition and reboots.

### How long it takes

Measured on four Pi 3 B over 100 Mbit ethernet, 2026-08-29:

| command | time |
|---|---|
| `prepare` (full, warm cache) | ~15 s |
| `adopt <node>` | ~45 s |
| `dryrun <node>` | ~2 min |
| `bake` | ~16 min (capture ~4 min of it) |
| `flash <node>` | ~6 min |
| `flash all` | ~7 min — parallel, and nodes already on the golden build are skipped in ~1 s |
| `status` | ~2 s |

`flash` skips any node already running the golden build; pass `-force` to
reflash it anyway. The golden is baked with a small rootfs
(`image.rootfs_size_gb`, 4 GiB) so captures and flashes stay short, and every
clone grows its filesystem to the whole card on first boot.

### sudo

`ssh.sudo` in `rasputin.yaml` picks how the CLI escalates:

```yaml
ssh:
  sudo: passwordless   # default: require NOPASSWD, fail fast without it
  # sudo: password     # fall back to `sudo -S` on nodes that lack it
```

`passwordless` is the better setup — grant it once per node and the CLI
never needs a secret at all:

```sh
ssh <user>@<node> "echo '<user> ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/<user>"
```

With `sudo: password`, the password is taken from `$RASPUTIN_SUDO_PASSWORD`,
or prompted for once (without echo) if that is unset. **It is deliberately
not a config field**: `rasputin.yaml` is committed to git, and a password in
git is a password published.

```sh
RASPUTIN_SUDO_PASSWORD="$(pass show cluster/sudo)" go run ./cmd/rasputin adopt all
```

## Configuration

Everything is driven by one file, `rasputin.yaml`. Pass `-c <path>` before
the command to use a different one (`rasputin -c other.yaml status`).

```yaml
cluster: rasputin        # name, used in logs
server:
  port: 8080             # HTTP port for serving images and receiving captures.
                         # 0 picks a random free port. The bind address is
                         # always auto-detected from the default route.
image:
  source_url: https://downloads.raspberrypi.com/raspios_lite_arm64_latest
  rootfs_size_gb: 4      # rootfs cap for the BAKED golden only. Small cap =
                         # short captures and flashes; every clone grows its
                         # filesystem to the whole card on first boot.
ssh:
  key: ~/.ssh/id_ed25519 # the agent is tried first, then this file
  users: [berry]         # SSH users, tried in order
  sudo: passwordless     # or `password` — see the sudo section
provision:               # what every node ends up with
  user: berry
  authorized_keys: ~/.ssh/id_ed25519.pub
  timezone: America/Sao_Paulo
  locale: en_US.UTF-8
  packages: [podman, curl, htop, vim, git, tmux]
builder: rasputin001     # the node `bake` wipes and captures
timeouts:
  flash_minutes: 25
  bake_minutes: 45
nodes:                   # the MAC is the node's identity; names and IPs
  - { name: rasputin001, mac: "b8:27:eb:01:02:03" }   # are convenience
  - { name: rasputin002, mac: "b8:27:eb:04:05:06" }
```

Anything under `image:` or `provision:` is baked into the image, and the
values are consumed at **prepare** time, not bake time. After editing them,
always run the full sequence — `prepare`, then delete the stale
intermediate, then `bake`:

```sh
go run ./cmd/rasputin prepare && rm -f out/vanilla-custom.img
go run ./cmd/rasputin bake
go run ./cmd/rasputin flash all
```

## Commands

Nodes are named by their config `name`, or `all` for every configured node.

### `prepare` — build the artifacts

Builds `out/recovery.gz` (the recovery agent initramfs) and turns the
cached stock Raspberry Pi OS image into `out/vanilla-custom.img.zst` with a
customised boot partition. ~15 s with a warm cache; the first run downloads
the stock image (~500 MB) into `cache/`. Harmless to re-run, required after
any `rasputin.yaml` change.

- `-initramfs-only` — stop after `recovery.gz` (this is what `make build`
  runs).

### `adopt <node|all>` — bring a live node under management

Installs the recovery mechanism on a node that already runs an OS and
answers SSH: copies `recovery.gz` to the boot partition, adds the
`initramfs` line to `config.txt` (backing up the original as
`config.txt.pre-rasputin`), then reboots the node once to prove it still
boots. Does **not** flash anything. ~45 s per node.

- `-reboot-check=false` — skip the verification reboot.

### `dryrun <node|all>` — rehearse without risk

Makes the node download and decode the entire image exactly as a flash
would, into a discard writer. The card is never opened for writing. The
node leaves a `reflash-dryrun.log` report on its boot partition, which the
CLI prints. ~2 min.

- `-vanilla` — rehearse with the prepared stock image even when a golden
  image exists.

### `bake` — build the golden image

**Wipes the `builder:` node.** Reflashes it with the prepared stock image,
waits for it to provision itself (packages, user, timezone), runs the seal
script that strips everything unique (host keys, machine-id, logs), then
has the recovery agent stream the card back as `out/golden.img.zst`. The
upload is fully decoded and checksum-verified before it can replace a
previous golden, and the bake only reports success after the builder
reboots into its own sealed system and passes the same health checks a
flashed clone gets. ~16 min end to end, of which the capture is ~4 min.

### `flash <node...|all>` — clone the golden image

Writes the golden image to each target node and verifies the result: build
id matches, hostname reapplied by the identity service, systemd settles.
Nodes flash **in parallel** — each is independent, and a node that fails is
left safely in the retrying recovery agent, not half-written. A node
already running the golden build is skipped in about a second, so
`flash all` is always safe to reach for.

- `-force` — reflash a node even when it already runs the golden build.

~6 min per node. Up to two nodes flash at full speed simultaneously; three
or four share the 100 Mbit wire and each slows by roughly a quarter, which
still beats flashing them one after another. After a successful flash the
CLI clears your `~/.ssh/known_hosts` entries for the node, so plain `ssh`
keeps working despite the regenerated host keys.

### `status` — read-only health table

Node, address, SSH user, hostname, build id, provisioned marker, uptime,
for every configured node. ~2 s. Touches nothing.

### `serve` — the HTTP server alone

Runs the image/capture server without triggering anything, for
hand-triggered flashes (see the escape hatch below). Prints the exact
flag-file line to paste.

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
go run ./cmd/rasputin prepare && rm -f out/vanilla-custom.img
go run ./cmd/rasputin bake     # wipes and rebuilds the builder
go run ./cmd/rasputin flash all
```

The `rm` matters: the decompressed intermediate is 2.9 GB of regenerable
disk, and a bake without a fresh `prepare` would silently reuse values from
the previous config (see *Configuration*).

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

**`REMOTE HOST IDENTIFICATION HAS CHANGED` after a flash.** The CLI clears
your `known_hosts` entries itself after a successful flash, so you should not
normally see this. If you do — a flash that failed late, or a node reached by
an address the CLI has not seen — `ssh-keygen -R <host>` clears it.

**`sudo needs a password and none was supplied`.** The node has no NOPASSWD
rule. Either grant one (see *sudo* above) or set `ssh.sudo: password`.

## Security trade-offs

These are deliberate, and worth knowing before you point this at a network
you do not own.

- **Host keys change on every flash.** A cloned node must not share a
  fingerprint with its siblings, so the golden image ships with none and each
  node generates its own on first boot. The CLI therefore cannot use
  `known_hosts`: it pins each node's key in `out/state.json` on first sight
  and drops the pin itself whenever *it* is the one replacing the system. A
  key that changes at any other time is reported as an error. After a
  successful `flash` or `bake` the CLI also runs `ssh-keygen -R` against your
  own `~/.ssh/known_hosts` for that node's names and addresses, so plain
  `ssh` keeps working; `ssh-keygen` writes its usual `.old` backup.
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
