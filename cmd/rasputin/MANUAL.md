# rasputin manual

`rasputin` reflashes a headless Raspberry Pi cluster over the network. Every
node ends up a byte-identical clone of one *golden image*, and every node can
be rebuilt remotely, including the node the golden image was baked on.

This manual is written so that a person or a program can read it once and
operate the tool correctly. It is compiled into the binary: `rasputin manual`
prints it, `rasputin manual -man` renders it for `man`, and
`rasputin manual -json` returns it with the command list. The source is
`cmd/rasputin/MANUAL.md`; the README explains the design.

## 1. The mental model in five sentences

1. **`rasputin.yaml` is the desired state**: which nodes exist (by MAC), which
   user, packages and rootfs size they get, which node is the builder.
2. **`prepare`** turns that config into artifacts on the build host
   (`out/recovery.gz`, `out/vanilla-custom.img.zst`). It never touches a node.
3. **`adopt`** installs a tiny recovery agent on a live node's boot partition,
   so that from then on the node can be reflashed with nothing but a flag
   file and a reboot.
4. **`bake`** wipes the *builder* node, lets it provision itself from the
   prepared image, strips it of everything unique, and streams its card back
   as `out/golden.img.zst`.
5. **`flash`** clones the golden image onto nodes, in parallel; **`status`**
   reads them back; **`sync`** does all of the above, skipping whatever is
   already current, and is the normal way to drive the cluster.

Nodes are addressed by their config `name`, or `all`. The MAC in the config is
the identity that matters: every connection verifies
`/sys/class/net/eth0/address` and refuses to act on a mismatch.

## 2. What is safe, what is destructive

| command | touches | destructive? | typical time |
|---|---|---|---|
| `manual` | nothing | no | instant |
| `init` | writes `rasputin.yaml` | no (refuses to overwrite without `-force`) | instant |
| `status` | reads nodes over SSH | **no** | ~2 s |
| `sync -plan` | reads nodes, reads `out/` | **no** | ~5 s |
| `prepare` | `out/`, `cache/` | no | ~15 s warm, +4 min on first download |
| `serve` | listens on a port | no (a node only reflashes if *you* write its flag file) | until interrupted |
| `dryrun` | reboots the node into the recovery agent and back, never writes its card | no | ~2 min per node |
| `adopt` | edits the boot partition, reboots once | low: reversible, keeps the OS | ~45 s per node |
| `bake` | **wipes the builder node** | **yes** | ~16 min |
| `flash` | **wipes every target node** not already on the golden build | **yes** | ~6 min per node, parallel |
| `sync` | any of the above, as needed | **yes when it bakes or flashes** | 5 s (no-op) to ~25 min |

Rules a program driving this tool must follow:

- **Never wipe without the owner's say-so.** `bake`, `flash` and any `sync`
  whose plan lists wipes destroy SD cards. Show the owner the plan
  (`sync -plan -json` gives it as data) and get an explicit yes before running
  `sync -yes`, `bake` or `flash`.
- `sync` **asks before wiping** and, without a terminal or with `-json`, it
  refuses instead of guessing. `-yes` is the only way to run it unattended.
- Read-only questions are `status` and `sync -plan`. Use those first.
- Aborting `sync`, `flash` or `bake` with ctrl-c is safe: a node caught
  mid-flash stays in the retrying recovery agent, never half-written, and the
  next `sync` resumes.
- Do not run two node-touching commands at once. They share the HTTP port
  and the nodes.

## 3. Invocation and output contract

```
rasputin [-c rasputin.yaml] [-json] <command> [flags] [args]
```

- `-c <path>` picks the config (default `./rasputin.yaml`). It goes *before*
  the command.
- Flags go **before** positional arguments (Go flag parsing stops at the
  first non-flag). `-flag` and `--flag` are both accepted.
- `-h` on any command prints its usage to stderr and exits 0.
- **Exit status**: 0 means the command succeeded, 1 means it did not. There
  are no other codes. A failure prints `rasputin: <error>` on stderr.
- Text mode prints tables and progress lines to stdout, prompts to stderr.

### `-json`

Every command accepts `-json` (before or after the command name). It changes
stdout into a stream of **newline-delimited JSON objects** and nothing else:

- One object per line. Every object has a `type` field.
- The **last line is always** `{"type":"result","command":"<name>","ok":true|false,...}`.
  On failure it also carries `"error":"<message>"`, and the process exits 1.
  This line is written even when the failure happens before the command
  starts (a config that does not load, an unknown node).
- Before the result, a command may stream `{"type":"log","message":...,"time":...}`
  progress lines. `sync` also streams `plan` and `event` objects; `serve`
  streams one `serving` object (see the command sections).
- Durations are numbers of seconds (`duration_seconds`), times are RFC 3339,
  byte counts are integers, and an absent string field means empty.
- `-json` implies `-plain` for `sync` and never prompts. Usage text and the
  sudo password prompt still go to stderr.

A robust consumer reads every line, parses each as JSON, keeps the one whose
`type` is `result`, and checks `ok` and the exit status.

```sh
rasputin status -json | jq -c 'select(.type=="result") | .nodes[] | {name, reachable, build_id}'
rasputin sync -plan -json | jq 'select(.type=="plan") | {needs_confirmation, wipes}'
```

### sudo

Nodes need `sudo`. With `ssh.sudo: passwordless` (default) NOPASSWD must be
granted on the node. With `ssh.sudo: password` the password comes from
`$RASPUTIN_SUDO_PASSWORD` or an interactive prompt; without a terminal and
without the variable, the command fails with a message that names the
variable. It is never read from the config file.

## 4. Procedures

**First contact with an existing cluster** (nodes already run an OS you can SSH into):

```sh
rasputin init -node pi01=b8:27:eb:... -node pi02=b8:27:eb:... -builder pi01
$EDITOR rasputin.yaml          # user, key, packages, rootfs size
rasputin status                # every node must be reachable
rasputin sync -plan            # look at what would happen; it will wipe the builder
rasputin sync                  # confirm with y (or -yes); ~25 min
```

**Change the config (add a package, a node, a new OS image)**:

```sh
$EDITOR rasputin.yaml
rasputin sync -plan            # prepare+bake+flash all, or less
rasputin sync -yes
```

**Check on the cluster** (read-only): `rasputin status -json`.

**Recover one broken node**: `rasputin flash <node>` if it still boots and
answers SSH. If it does not, start `rasputin serve` and follow the
escape-hatch and troubleshooting sections of the README: the node's
recovery agent retries the download forever and never opens the card until
the image is reachable.

**Changed the recovery agent's Go code** (`internal/agent`, `internal/initramfs`):
`sync` cannot see that, so run `rasputin sync -force-prepare`.

**Run unattended / from a program**: `rasputin sync -json -yes` after the
owner has approved the plan from `rasputin sync -plan -json`.

## 5. Commands

Each section lists: what it does, flags, positional arguments, what it
touches, what the result object contains beyond the envelope
(`type`, `command`, `ok`, `error`).

### `manual [-man]`

Prints this manual, as Markdown. Needs no config.

- `-man` prints it as a man page instead, for `man`:
  `rasputin manual -man | man -l -` on Linux, or
  `rasputin manual -man > rasputin.1 && man ./rasputin.1` anywhere.
  `make install-man` installs it so that `man rasputin` works.
- **Result fields**: `manual` (the Markdown text), `man` (the roff, only
  with `-man`), `commands` (list of `{name, usage, short}` in the order the
  usage screen shows them).

### `init [-force] [-builder <name>] [-node <name>=<mac>]...`

Writes a commented starter `rasputin.yaml` to the `-c` path. Needs no
config. Refuses to overwrite an existing file unless `-force`.

- `-node name=mac` (repeatable) replaces the default four placeholder nodes
  `rasputin001..004`. Placeholder MACs `00:00:00:00:00:0N` are refused by
  every other command, by design, until replaced with the real ones from
  `cat /sys/class/net/eth0/address` on each Pi.
- `-builder` names the node `bake` wipes (default: the first node). It must
  be one of the nodes.
- **Result fields**: `path`, `nodes` (`[{name, mac}]`), `builder`,
  `placeholder` (true when the file still needs real MACs), `next` (the
  suggested next commands, as strings).

### `status [node...]`

Probes the named nodes (default `all`) in parallel over SSH. Read-only, safe
at any moment, even during a flash. Exit status is 0 as long as the probe
ran; a down node is reported in the data, not as a failure.

- **Result fields**: `nodes`, a list of
  - `name`, `mac`: as configured.
  - `reachable` (bool); when false, `error` says why, `ip` is the
    last-seen address if one is known, and the other fields are absent.
  - `ip`, `ssh_user`, `hostname`, `uptime`.
  - `build_id`: the golden build the node runs; absent when it runs a stock
    OS.
  - `adopted`: boots through the recovery agent.
  - `provisioned`: its first-boot package install has completed.

A healthy cluster has every node `reachable`, `adopted`, `provisioned` and
on the same `build_id` as `out/meta/golden.json`.

### `sync [flags]`

The idempotent whole-pipeline command. It probes every node (and refuses to
start if any is unreachable), derives a plan, prints it, asks before wiping,
runs only the stale steps in order (`probe`, `prepare`, `adopt`, `bake`,
[`dryrun`], `flash`, `status`), and stops at the first failure.

| flag | effect |
|---|---|
| `-plan` | probe, print the plan, exit 0. Touches nothing. |
| `-yes` | do not ask before wiping. Required for `-json` runs that wipe. |
| `-force` | `-force-prepare -force-bake -force-flash` |
| `-force-prepare` | rebuild the artifacts even when the fingerprint matches (needed after changing the agent's Go code) |
| `-force-bake` | rebake the golden even when current (**wipes the builder**) |
| `-force-flash` | flash every node even when already on the golden build |
| `-rehearse` | insert a `dryrun` of every node before the flash |
| `-plain` | one line per event instead of the live display |
| `-json` | plan, events and result as JSON objects (implies `-plain`, never prompts) |
| `-log <path>` | run log (default `out/sync.log`; `-` disables) |

A step is skipped when its output is current: `prepare` when the fingerprint
of `rasputin.yaml` plus the rendered provision files matches
`out/meta/prepare.json`; `adopt` when every node is adopted; `bake` when
`out/meta/golden.json` was built from the current prepare; `flash` per node
when its `build_id` equals the golden's.

In text mode the plan ends with a `WILL WIPE:` line naming every node that
loses its card, and an `estimated total`. A plan that wipes a node then
asks `Proceed? [y/N]`, unless `-yes` was given. Without a terminal, or with
`-json`, there is no prompt: the command fails with a message naming
`-yes`. Declining exits 1 with `cancelled; nothing was touched`. Ctrl-c
exits 1 with `aborted; ...` and the nodes are safe.

**JSON stream**, in order:

1. `{"type":"plan", ...}` once, before anything runs:
   - `cluster`, `needs_confirmation` (bool), `estimate_seconds`.
   - `wipes`: `[{node, step}]`, every node that loses its card. Empty means
     the run is non-destructive.
   - `steps`: `[{id, title, skip, reason, nodes, wipes, estimate_seconds}]`
     in execution order. `skip` true means it will not run and `reason` says
     why.
   - `status`: the probe, same shape as `status`'s `nodes`.
2. With `-plan`, the result follows at once:
   `{"type":"result","command":"sync","ok":true,"plan_only":true}`.
3. Otherwise, `{"type":"event", ...}` lines while it runs:
   - `kind`: `started`, `done`, `skipped`, `failed` (one step's lifecycle),
     `log` (a free-form line), `phase` (a sub-stage of a long step),
     `transfer` (byte progress, about once a second per node).
   - `step`: `probe`, `prepare`, `adopt`, `bake`, `dryrun`, `flash`, `status`.
   - `node`: set when the event is about one node.
   - `message`; for `transfer`: `bytes`, `total` (absent when unknown),
     `rate_bytes_per_second`.
   - `time`.
4. The result:
   - `ok`, `error` (the first failure, or the abort message).
   - `aborted`: true when the run was cancelled rather than failed.
   - `steps`: `[{id, title, skipped, reason, note, duration_seconds, error}]`.
     A step after a failure is `skipped` with reason `a previous step failed`.
   - `status`: the health table read back **after** the run. Empty when the
     run stopped before the status step, so a stale pre-run probe is never
     presented as the final state.
   - `duration_seconds`, `log` (the run log path, when enabled).

### `prepare [-initramfs-only]`

Builds `out/recovery.gz` (the recovery agent initramfs) and
`out/vanilla-custom.img.zst` (the stock image with a customised boot
partition) and records `out/meta/prepare.json`. Downloads the stock image
into `cache/` on first use. Touches nothing on the network but that download.
Everything under `image:` and `provision:` in the config is consumed here,
so a config edit needs a `prepare` before the next `bake`. Leaves the raw
`out/vanilla-custom.img` (~3 GB) on disk for a day-0 `dd`; `sync` deletes it.

- `-initramfs-only` stops after `recovery.gz` (`make build` uses this).
- **Result fields**: `initramfs_only`, `meta_path`, and the contents of
  `out/meta/prepare.json`: `build_id`, `base_image`, `base_url`,
  `image_path`, `image_bytes`, `zst_path`, `zst_bytes`, `sha256_img`,
  `sha256_zst`, `recovery_path`, `sha256_recovery`, `prepared_at`,
  `fingerprint`.

### `adopt [-reboot-check=false] <node|all>`

Installs the recovery mechanism on a node that runs an OS and answers SSH:
copies `recovery.gz` to its boot partition, adds the `initramfs` line to
`config.txt` (backup at `config.txt.pre-rasputin`) and reboots it once to
prove it still boots. Runs a preflight first (architecture, boot partition
mounted, free space, sudo). Nodes are done one at a time. Does not flash.
Requires `prepare` to have run.

- `-reboot-check=false` skips the verification reboot.
- **Result fields**: `reboot_check`, `adopted`, `failed`, `nodes`:
  `[{node, ok, rebooted, error, preflight: {user, host, checks: [{name, ok, detail}]}}]`.
- Exit 1 when any node failed; `error` is `N of M node(s) could not be adopted`.

### `dryrun [-vanilla] <node|all>`

Makes the node run the whole reflash pipeline (download, decode, checksum)
into a discard writer. Its card is never opened for writing; it reboots into
the recovery agent and back into its own system. Serves the golden image if
one exists, else the prepared stock image. The node leaves
`reflash-dryrun.log` on its boot partition, which is returned verbatim.

- `-vanilla` rehearses with the stock image even when a golden exists.
- **Result fields**: `image` (`{name, path, url}`), `passed`, `failed`,
  `nodes`: `[{node, ok, report, error}]`.
- Exit 1 when any node failed.

### `bake`

**Wipes the builder node** (`builder:` in the config). Reflashes it with the
prepared stock image, waits for it to provision itself, seals it (removes
host keys, machine-id, logs), captures its card as `out/golden.img.zst`,
verifies the capture decodes to exactly the used part of the card, records
`out/meta/golden.json`, and finally reboots the builder into the sealed
system and health-checks it. Takes no arguments. Needs about 5 GB free on
the build host.

- **Result fields**: `duration_seconds`, `golden_path`, `meta_path`,
  `golden`: `{build_id, sha256, bytes, base, builder, baked_at, card_used_bytes}`.

### `flash [-force] <node...|all>`

**Wipes every target** that is not already on the golden build. Serves
`out/golden.img.zst`, writes the reflash flag on each node, reboots it into
the recovery agent, waits for it to come back, and verifies build id and
hostname. Nodes flash in parallel. A node already on the golden build is
skipped in about a second unless `-force`. Clears the node's stale entries
from `~/.ssh/known_hosts` afterwards. Requires a `bake`.

- **Result fields**: `golden` (`{build_id, bytes, path, url}`),
  `timeout_minutes`, `force`, `flashed`, `skipped`, `failed`, `nodes`:
  `[{node, ok, skipped, build_id, hostname, duration_seconds, error}]`.
- Exit 1 when any node failed; the others are still reported.

### `serve`

Runs the built-in HTTP server alone, offering whichever of
`out/golden.img.zst` and `out/vanilla-custom.img.zst` exist, until ctrl-c or
SIGTERM. For triggering a reflash by hand or debugging a node that will not
download.

- **JSON stream**: first `{"type":"serving","images":[{name,path,url}],"reflash_flag":...,"trigger":...}`
  as soon as the server is up (`trigger` is the shell command that reflashes
  a node by hand), then, on shutdown, the result with `images`.

## 6. Files

| path | what |
|---|---|
| `rasputin.yaml` | the config (gitignored: it holds the nodes' identities) |
| `out/recovery.gz` | the recovery agent initramfs |
| `out/vanilla-custom.img[.zst]` | the prepared stock image; the raw one is regenerable and large |
| `out/golden.img.zst` | the golden image every flash clones |
| `out/meta/prepare.json`, `out/meta/golden.json` | provenance; `sync` plans from these |
| `out/state.json` | pinned SSH host keys and last-seen IPs (mode 0600) |
| `out/sync.log` | append-only log of every `sync` event |
| `cache/` | downloaded stock images |

`out/`, `cache/` and `rasputin.yaml` are not committed.

## 7. Errors you will see, and what they mean

- `node … still carries the placeholder mac …`
  `rasputin.yaml` still has `init`'s placeholders; fill in the real MACs.
- `N of M node(s) do not answer … nothing has been touched`
  `sync` refuses to plan with a node down; fix the node or remove it from
  the config.
- `this plan wipes at least one node and … re-run with -yes`
  No prompt is possible (no terminal, or `-json`). Get the owner's approval
  of the plan, then re-run with `-yes`.
- `cancelled; nothing was touched`
  The operator answered no.
- `aborted; the nodes are still in the recovery agent, nothing is half-written`
  Ctrl-c. Safe to re-run `sync`; it resumes where it stopped.
- `… is missing: run rasputin bake first`, `… run rasputin prepare first`
  Artifact order: `prepare`, then `bake`, then `flash`.
- `sudo needs a password and none was supplied`
  Grant NOPASSWD on the node, or set `ssh.sudo: password`.
- `ssh.sudo is "password" but there is no terminal to prompt on`
  Set `RASPUTIN_SUDO_PASSWORD`.
- `… answers as MAC …, but … is … — refusing to touch it`
  The machine answering to that name is not the configured node. Stop and
  investigate before doing anything else; nothing was written.
- `REMOTE HOST IDENTIFICATION HAS CHANGED` (from plain `ssh`, not rasputin)
  A node was reflashed and regenerated its host keys; `ssh-keygen -R <host>`.
