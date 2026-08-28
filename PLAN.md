# rasputin — Pi 3 cluster reflash CLI: execution plan

Mission: a Go CLI tool (`rasputin`) that reads `rasputin.yaml` and fully automates
remote reflashing of a 4-node Raspberry Pi 3 cluster: builds a recovery initramfs,
prepares a customized image, bakes a golden image using a real Pi as the arm64
builder, and flashes any node over the network with one command. No Docker, no
loop mounts, no physical access after a node is adopted.

This plan is designed to be executed iteratively (e.g. via `/loop`), by a weaker
model, with independent tasks run in parallel. **Read this file top to bottom
before doing anything.** All context you need is in this file, `plan/FACTS.md`,
and the per-phase task files. You do NOT have the conversation that produced
this plan — do not guess beyond what is written; `plan/FACTS.md` is ground truth.

## Loop protocol (follow exactly, every iteration)

1. Read the task board below, `tail -40 plan/PROGRESS.md`, and `plan/BLOCKERS.md`.
2. Pick the first task with status `todo` whose `deps` are all `done`.
   Tasks marked `[P]` and sharing a group letter are independent of each other:
   you may run several of them concurrently via subagents (give each subagent
   the task file path plus `plan/FACTS.md` and tell it to follow the task VERIFY
   block). Never run two tasks that touch the same package concurrently.
3. Set the task to `doing` in the board (edit this file), do the work exactly as
   specified in its task file, then run every command in the task's `VERIFY`
   block. All must pass. Do not weaken, skip, or delete a VERIFY step; if a
   VERIFY step is impossible as written, that is a blocker.
4. On success: set status `done`, append one entry to `plan/PROGRESS.md`
   (`date, task id, what was done, anything learned worth keeping`), then
   `git add -A && git commit -m "<task-id>: <short summary>"`.
5. On failure after 2 genuine attempts: set status `blocked`, describe the
   blocker and what you tried in `plan/BLOCKERS.md`, commit, move on to the
   next runnable task.
6. Stop the loop when every task is `done`, or when only `blocked` tasks
   remain. Then write a summary at the end of `plan/PROGRESS.md`.

## Hard guardrails

- **Hardware tasks (phase 4) are strictly sequential** — never parallel, never
  out of order, never while another phase-4 task is `doing`. Software phases
  0–3 must be fully `done` first (except explicitly noted).
- Never adopt/flash a node not named by the current task. After any action
  that reboots a node, if the node is not back on SSH within the task's
  timeout, STOP phase 4 entirely, write a blocker, and continue only with
  software tasks. The owner has physical access and will recover it.
- Adoption keeps a backup: before editing a node's `config.txt`, copy it to
  `config.txt.pre-rasputin` on the node's boot partition.
- Never run `apt upgrade`, never touch nodes' data beyond what a task says.
- Do not `git push` (no remote). Do not commit files in `out/` or `cache/`
  (`.gitignore` covers this).
- If a design decision seems wrong, do not redesign: write it in
  `plan/BLOCKERS.md` under "design concerns" and keep going if possible.

## Architecture (decided — build this, don't redesign)

- **One Go module**, CLI `rasputin` + recovery agent, minimal deps:
  `gopkg.in/yaml.v3`, `golang.org/x/crypto/ssh`, `github.com/klauspost/compress`
  (zstd), `github.com/ulikunitz/xz`, `github.com/diskfs/go-diskfs`,
  `github.com/insomniacslk/dhcp`, `github.com/vishvananda/netlink`.
  No Docker/containers anywhere.
- **Recovery agent** (`cmd/agent`): a single static Go binary that IS the
  initramfs `/init` (like u-root/gokrazy). Cross-compiled with
  `CGO_ENABLED=0 GOOS=linux GOARCH=arm64`. Packed into `recovery.gz` (newc cpio
  with just `/init` + a crafted `/dev/console` char-5:1 entry, gzipped) by our
  own tiny cpio writer. Loaded on every boot via `initramfs recovery.gz
  followkernel` in config.txt. Modes, chosen by flag files on the boot
  partition (first line of the flag file, if non-empty, is the URL to use):
  - no flag → mount rootfs ro, switch_root into `/sbin/init` (normal boot)
  - `reflash` → DHCP, HTTP-GET image, zstd-decode, write whole `/dev/mmcblk0`,
    hard reboot. Retry forever with backoff; card untouched until stream opens.
  - `reflash-dryrun` → same pipeline into a discard writer, 3 attempts, write
    report to boot partition, remove flag, reboot.
  - `capture` → parse MBR, stream partitions 1+2 as zstd to the CLI via HTTP
    POST (this is how golden images are made), remove flag, reboot.
- **Image customization happens ONLY on the FAT32 boot partition** (pure-Go
  writes via go-diskfs; no ext4 writes, ever). All rootfs changes are done on
  the Pi itself by a generated `firstrun.sh` triggered through the official
  Raspberry Pi Imager mechanism: `systemd.run=/boot/firstrun.sh ...` appended
  to `cmdline.txt`. firstrun creates the user, hardens sshd, sets
  timezone/locale, installs the identity service, resizes rootfs to a fixed
  cap, sets up a provision service that apt-installs packages, then removes
  itself from cmdline.txt.
- **Golden image is baked ON a Pi** (`builder` node in YAML): reflash builder
  with the prepared vanilla image → firstrun + provision run → CLI "seals" it
  over SSH (purge SSH host keys, truncate machine-id) → `capture` flag →
  agent streams the provisioned card back → `out/golden.img.zst`. Every later
  flash of any node is a byte-identical clone of that. Identity
  (hostname per MAC from `nodes.conf` on the boot partition) is applied on
  every boot by a baked oneshot service; host keys/machine-id self-regenerate.
- **CLI orchestration**: built-in HTTP server (serves images, receives
  captures, counts bytes per client = progress); SSH triggering
  (`write flag; reboot`); polling; parallel multi-node flash with per-node
  status; `adopt` installs the recovery mechanism onto a live stock node over
  SSH (that is how the current cluster gets onboarded with no physical access).

## CLI surface

```
rasputin [-c rasputin.yaml] <command>
  prepare              build recovery.gz + vanilla image with custom boot partition
  adopt <node|all>     install recovery mechanism on a live node via SSH
  dryrun <node|all>    validate download+decode pipeline on a node, harmless
  bake                 produce out/golden.img.zst using the builder node
  flash <node...|all>  reflash node(s) from golden image, report timing
  status               table: node, ip, reachable, hostname, build-id, uptime
  serve                run the HTTP server standalone (debugging)
```

## Task board

Status: todo | doing | done | blocked. Edit in place.

| id  | task                                            | phase | deps        | par | status |
|-----|-------------------------------------------------|-------|-------------|-----|--------|
| T01 | Module scaffold, YAML config, .gitignore        | 0     | —           |     | done   |
| T02 | cpio newc writer package                        | 0     | T01         | [P]A| done   |
| T03 | kmsg/log + MBR parser packages                  | 0     | T01         | [P]A| done   |
| T04 | initramfs assembler (build agent, pack, verify) | 0     | T02,T05     |     | todo   |
| T05 | agent: boot, mounts, flags, switch_root         | 1     | T01,T03     | [P]A| done   |
| T06 | agent: network up (netlink + DHCP)              | 1     | T05         |     | todo   |
| T07 | agent: reflash/dryrun/capture pipelines         | 1     | T05,T06     |     | todo   |
| T08 | vanilla image download + xz decode + cache      | 2     | T01         | [P]A| todo   |
| T09 | FAT32 boot-partition editor (go-diskfs)         | 2     | T08         |     | todo   |
| T10 | firstrun.sh / identity / provision templates    | 2     | T01         | [P]A| todo   |
| T11 | `prepare` command (vanilla + boot mods + zstd)  | 2     | T04,T09,T10 |     | todo   |
| T12 | HTTP server (serve, capture, progress)          | 3     | T01         | [P]B| todo   |
| T13 | SSH client + node resolution + state cache      | 3     | T01         | [P]B| todo   |
| T14 | `adopt`, `dryrun` commands                      | 3     | T11,T12,T13 |     | todo   |
| T15 | `flash`, `bake`, `status` commands              | 3     | T14         |     | todo   |
| T16 | hardware gate: preflight + sudo fix check       | 4     | T15         |     | todo   |
| T17 | HW: adopt rasputin001 + reboot-survives check   | 4     | T16         |     | todo   |
| T18 | HW: dryrun on rasputin001                       | 4     | T17         |     | todo   |
| T19 | HW: bake golden on rasputin001                  | 4     | T18         |     | todo   |
| T20 | HW: flash rasputin001 from golden, verify       | 4     | T19         |     | todo   |
| T21 | HW: adopt+flash remaining reachable nodes       | 4     | T20         |     | todo   |
| T22 | README, cleanup, final end-to-end timing report | 5     | T21         |     | todo   |

Task details: `plan/00-foundation.md` (T01–T04), `plan/01-agent.md` (T05–T07),
`plan/02-image.md` (T08–T11), `plan/03-orchestration.md` (T12–T15),
`plan/04-hardware.md` (T16–T21), `plan/05-docs.md` (T22).

Reference material: `legacy/` holds the earlier shell/busybox prototype. Its
`legacy/initramfs/init` is the behavioral spec for the agent (safety ordering,
retry semantics, logging); `legacy/initramfs/build.sh` documents the crafted
cpio `/dev/console` entry bytes. Do not run legacy scripts; port their logic.
