# rasputin

Reflash a headless Raspberry Pi cluster over the network, with one command
and no physical access.

```
rasputin sync
```

Every node is a byte-identical clone of one golden image, and every node can
be rebuilt from scratch remotely — including the node the golden image was
baked on. `sync` works out what is already current and does only the rest, so
that one command is both the first build and the daily no-op. There is no
Docker, no loop mount, no `sudo` on the build host, and nothing to plug in
after a node has been adopted once.

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

Run it from a checkout of the repository: `prepare` compiles the recovery
agent from `./cmd/agent`, so the CLI expects the module root as its working
directory. (`go install github.com/ericovis/rasputin/cmd/rasputin@latest`
gives you the binary, and everything that does not build an image works from
anywhere, but `prepare`, `bake` and `sync` need the source tree.)

```sh
git clone https://github.com/ericovis/rasputin.git
cd rasputin
go run ./cmd/rasputin init   # once: writes a commented rasputin.yaml
$EDITOR rasputin.yaml        # MACs, users, packages, builder
go run ./cmd/rasputin sync     # everything else
```

`sync` probes the cluster, prints the plan it derived, asks before anything is
wiped, and then runs only the steps whose outputs are stale — prepare, adopt,
bake, flash, status. Run it again after editing `rasputin.yaml` and it rebuilds
exactly what that edit invalidated. Run it on an unchanged cluster and it
finishes in seconds having touched nothing.

The individual commands it drives (`prepare`, `adopt`, `bake`, `flash`,
`dryrun`, `status`) are all still there under *Commands*: they are the building
blocks, and the escape hatch when `sync` decides something you disagree with.

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
| `sync` (nothing stale) | ~5 s — the probe and the final status table, nothing else |
| `sync` (config changed) | ~25 min — the sum of prepare + bake + flash all |

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
not a config field**: `rasputin.yaml` gets backed up, pasted and shared, and a
password in a config file is a password published.

```sh
RASPUTIN_SUDO_PASSWORD="$(pass show cluster/sudo)" go run ./cmd/rasputin adopt all
```

## Configuration

Everything is driven by one file, `rasputin.yaml`. Pass `-c <path>` before
the command to use a different one (`rasputin -c other.yaml status`).

The file is gitignored. It lists your nodes' MAC addresses, which is the
identity the CLI trusts, and that has no business in a public repository:
keep it in the checkout and back it up somewhere private.

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
  authorized_keys:        # one entry or a list; each is a .pub file path or
    - ~/.ssh/id_ed25519.pub   # a public key written inline
    - ssh-ed25519 AAAA... you@laptop
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
values are consumed at **prepare** time, not bake time. Editing one and baking
without re-preparing silently bakes the *old* value. `sync` notices the edit and
runs the whole sequence in the right order:

```sh
go run ./cmd/rasputin sync
```

By hand it is prepare, delete the stale intermediate, bake, flash:

```sh
go run ./cmd/rasputin prepare && rm -f out/vanilla-custom.img
go run ./cmd/rasputin bake
go run ./cmd/rasputin flash all
```

## Commands

Nodes are named by their config `name`, or `all` for every configured node.
Flags always come before the positional arguments.

The full reference is the manual, `rasputin manual`, whose source is
[`cmd/rasputin/MANUAL.md`](cmd/rasputin/MANUAL.md): what every command
touches, how long it takes, its flags, and its JSON. It is written for a
program as much as for a person.

### `-json` — machine-readable output, on every command

Every command accepts `-json` (before or after the command name). Stdout then
carries newline-delimited JSON objects and nothing else: optional
`{"type":"log",...}` progress lines, `sync`'s `plan` and `event` objects, and
always, last, `{"type":"result","command":"...","ok":true|false,...}` with the
command's data, plus `"error"` on failure. The exit status is 1 on failure
either way, even when the failure is a config that does not load. `-json`
never prompts: a `sync` that would wipe a node needs `-yes`, and `-plan`
shows the plan without running it.

```sh
rasputin status -json | jq -c 'select(.type=="result") | .nodes[] | {name, reachable, build_id}'
rasputin sync -plan -json | jq 'select(.type=="plan") | {needs_confirmation, wipes}'
rasputin sync -json -yes | tee run.jsonl | jq -c 'select(.type=="result") | {ok, error}'
```

The field-by-field schema of each result is in the manual.

### `init` — write a starting `rasputin.yaml`

```sh
go run ./cmd/rasputin init
go run ./cmd/rasputin init -builder pi01 -node pi01=b8:27:eb:01:02:03 -node pi02=b8:27:eb:04:05:06
```

Writes a fully commented config with working defaults: the latest 64-bit
Raspberry Pi OS Lite, a 4 GiB baked rootfs, user `berry`, your
`~/.ssh/id_ed25519` key pair, passwordless sudo, and one package (`curl`).
Refuses to overwrite an existing file. `-c <path>` before the command chooses
where it is written.

- `-force` — overwrite an existing config.
- `-builder <name>` — the node `bake` will wipe (default: the first node).
- `-node <name>=<mac>` — repeatable; replaces the placeholder node list.

Without `-node` flags it writes four placeholders, `rasputin001`…`rasputin004`,
with the **placeholder MACs** `00:00:00:00:00:01`…`04`. Those are not valid
identities, and every node-touching command refuses to run while one is
present, naming the node in the error. Fill them in with the real ones:

```sh
ssh <node> cat /sys/class/net/eth0/address
```

The MAC is what the CLI trusts; the name and the IP are convenience. See
*Security trade-offs*.

### `sync` — do whatever is needed, and nothing else

```sh
go run ./cmd/rasputin sync
```

One idempotent pass over the whole pipeline. It first probes every node
(read-only, ~2 s; an unreachable node aborts before anything is touched), then
decides each step from what it found on disk and on the nodes:

| step | runs when | skipped when |
|---|---|---|
| `probe` | always — the plan is built from it | never |
| `prepare` | `rasputin.yaml` or a built-in template changed since the last prepare, or an artifact in `out/` is missing | the fingerprint recorded in `out/meta/prepare.json` still matches the config |
| `adopt` | a probed node has no recovery agent yet | every node is already adopted |
| `bake` | `prepare` will run, or there is no golden, or the golden came from an older prepare | `out/meta/golden.json`'s build id matches the current prepare's |
| `dryrun` | only with `-rehearse` | not in the plan at all otherwise |
| `flash` | per node: the node's build id differs from the golden's | that node already runs the golden build |
| `status` | always, last | never |

The prepare fingerprint is a hash of `image.source_url`, the rendered
provision scripts and `nodes.conf`, so pointing the config at a new stock
image, adding a package or adding a node re-prepares, and re-flowing a comment
does not.

The plan is printed before anything runs, each step as RUN or SKIP with its
reason, ending in a `WILL WIPE:` line naming every node that loses its
contents. If that line is non-empty, `sync` asks `Proceed? [y/N]` and does
nothing else without a `y`. With no terminal and no `-yes` it stops and says
so rather than guessing.

| flag | effect |
|---|---|
| `-force` | all three `-force-*` flags at once |
| `-force-prepare` | rebuild the artifacts even when the fingerprint matches |
| `-force-bake` | rebake the golden even when it is current (**wipes the builder**) |
| `-force-flash` | flash every target even when it already runs the golden build |
| `-rehearse` | run a `dryrun` on every node before the flash |
| `-yes` | answer the confirmation prompt |
| `-plan` | probe, print the plan, and exit without touching anything |
| `-plain` | line-by-line output instead of the TUI |
| `-json` | plan, events and result as JSON objects, one per line (implies `-plain`, never prompts) |
| `-log <path>` | where the run log goes (default `out/sync.log`; `-` disables it) |

On a terminal `sync` draws a live view: per-step state, a per-node bar during
flash, the current sub-stage during bake, and a tail of the log. **`q` and
`ctrl-c` are safe** — they cancel the run, and a node caught mid-flash is left
in the retrying recovery agent exactly as any other interruption would leave
it, never half-written. `-plain` (used automatically when stdout is not a
terminal) prints the same events one per line, which is what CI wants.

Either way every event except the polled byte counters is appended to
`out/sync.log` in the plain format, so the scrollback the TUI dropped is still on
disk. `-log` moves that file, and `-log -` turns it off. After the run, the per-step
summary is printed to stdout so it stays in your scrollback, followed by the
`status` table when the `status` step got to run — a run that stopped early
prints no health table rather than the pre-run probe, which would describe a
fleet that no longer exists.

`sync` deletes `out/vanilla-custom.img` once it has been compressed — it is 2.9 GB
of regenerable intermediate and `bake` only needs the `.zst`. Plain `prepare`
keeps it, because Day 0 writes it to a card with `dd`.

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
for every configured node. ~2 s. Touches nothing. With `-json` each node is
an object with `reachable`, `adopted`, `provisioned`, `build_id` and, for a
node that does not answer, `error`.

### `serve` — the HTTP server alone

Runs the image/capture server without triggering anything, for
hand-triggered flashes (see the escape hatch below). Prints the exact
flag-file line to paste.

### `manual` — the manual, from the binary

Prints `cmd/rasputin/MANUAL.md`, which is compiled in, so whoever holds
the executable has the whole reference. `manual -json` returns the text
and the command list as data. `manual -man` renders it as a man page:

```sh
rasputin manual -man | man -l -     # Linux
make install-man && man rasputin     # installs out/rasputin.1 into MANDIR
```

`MANDIR` defaults to `/usr/local/share/man/man1`, which is root-owned on
macOS, so either `sudo make install-man` or point it at Homebrew's
directory, which is on the man path and writable by you:

```sh
make install-man MANDIR=/opt/homebrew/share/man/man1
```

The installed page is a snapshot: rerun the install after editing
`cmd/rasputin/MANUAL.md`.

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
go run ./cmd/rasputin sync       # re-prepares, rebakes, reflashes — only what changed
```

That is the whole loop. `sync` sees the new fingerprint, so it re-prepares,
rebakes the golden (wiping the builder, after asking), and reflashes every node
whose build id no longer matches. Change nothing and run it again and it is a
few seconds of probe and status.

By hand the same thing is:

```sh
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

**`node … still carries the placeholder mac … written by rasputin init`.**
The node list is still the one `init` wrote. Read each Pi's real address with
`ssh <node> cat /sys/class/net/eth0/address` and put it in `rasputin.yaml`.
Nothing that touches a node runs until every MAC is real, because the MAC is
the only identity the CLI trusts.

**`sync` stopped at the probe.** A node did not answer, and `sync` will not begin a
multi-step run it cannot finish. Fix that node, or drop it from
`rasputin.yaml`, and run `sync` again — nothing was touched.

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
cmd/rasputin      the CLI; MANUAL.md is the embedded manual, json.go the JSON views
cmd/agent         the recovery agent — becomes /init inside recovery.gz
internal/agent    boot, flag selection, switch_root, reflash/dryrun/capture
internal/bootfs   FAT32 boot-partition editing and config.txt/cmdline.txt patches
internal/cluster  orchestration: adopt, dryrun, bake, flash, status
internal/cpio     newc writer and reader
internal/events   the progress contract between orchestration and any UI
internal/initramfs  cross-compiles the agent and packs it
internal/nodes    resolution (name/MAC/IP/all), MAC verification, preflight
internal/prepare  the prepare pipeline
internal/provision  firstrun/identity/provision/seal templates
internal/server   HTTP server: image serving, capture receipt, progress
internal/sshx     SSH client with user fallback and host-key pinning
internal/tui      the `sync` terminal UI (the only package that draws)
internal/up       planning and execution for `sync`: what is stale, in what order
internal/vanilla  stock image download, xz decode, cache
```

## Development

```sh
make build        # CLI + recovery.gz + out/rasputin.1
make test         # go test ./... and a linux/arm64 cross-build
make install-man  # man rasputin (MANDIR=/opt/homebrew/share/man/man1 on a Mac without sudo)
make clean
```

Tests run on macOS. Linux-only syscalls are behind build tags so the module
still vets and tests on the build host, and `GOOS=linux GOARCH=arm64 go build
./...` must always pass. Nothing in the test suite needs hardware: the agent
pipelines run against `httptest`, the SSH client against an in-process SSH
server, and image contents are verified by re-reading the FAT partition with
`go-diskfs` rather than by mounting anything.

`.github/workflows/test.yml` defines the hardware-free CI: `gofmt`, `go vet`,
`make test` and `make build` on Ubuntu, on every push to `main` and every
pull request, at [github.com/ericovis/rasputin](https://github.com/ericovis/rasputin).
Nothing that needs a Pi is in it: `sync`, `bake`, `flash`, `adopt`, `dryrun`,
`serve` and `status` all need the four nodes on the LAN and SSH to them, and
`flash`, `bake` and `sync` are destructive, so they stay a manual, on-hardware
step. A full `prepare` is left out too — it downloads ~500 MB and needs ~3 GB
free — which means `internal/prepare`'s `TestImageContents` skips there for
want of `out/vanilla-custom.img`. That gate is local only: run `go run
./cmd/rasputin prepare` and then `go test ./internal/prepare` before trusting
an image.
