# Ground-truth facts (verified 2026-08-29)

Do not re-derive these; trust them unless a VERIFY step proves otherwise.
If you discover one is wrong, update it here and note it in PROGRESS.md.

## The cluster

4× Raspberry Pi 3 Model B, wired ethernet, headless, DHCP on 192.168.0.0/24.
SSH works today as user **`berry`** (key `~/.ssh/id_ed25519`). `ericovis` no
longer exists on the nodes. mDNS `rasputinNNN.local` resolves, but never trust
it — the MAC is the identity.

All four run the golden image build **`20260829T205033Z-307368`** and have
**passwordless sudo**.

| node        | MAC (eth0)          | last IP        | sudo -n | state |
|-------------|---------------------|----------------|---------|-------|
| rasputin001 | `b8:27:eb:01:02:03` | 192.168.0.74   | YES     | builder; rootfs at the 4 GiB bake cap |
| rasputin002 | `b8:27:eb:04:05:06` | 192.168.0.124  | YES     | clone, rootfs grown to the full card |
| rasputin003 | `b8:27:eb:07:08:09` | 192.168.0.222  | YES     | clone, grown; duplicate hostname permanently fixed |
| rasputin004 | `b8:27:eb:0a:0b:0c` | 192.168.0.190  | YES     | clone, grown |

- MAC↔name above is the LIVE cluster state (verified over SSH). The owner once
  pasted a list implying 002/004 swapped; live state wins.
- **Historical (T15), now RESOLVED: rasputin003 once reported its hostname as
  `rasputin002`.** The identity service baked into the golden image fixed it
  permanently; 003 has reported its own name since T21 and again after the
  2026-08-29 reflash. The lasting lesson stands: **mDNS names can never be
  trusted on this cluster** — the MAC check in `internal/nodes` is what caught
  the duplicate, and a node whose name is wrong is only findable through ARP.
  Do not weaken that check.
- **The owner's SSH key is passphrase protected** and is used through the
  macOS SSH agent (SSH_AUTH_SOCK is set, `ssh-add -l` shows the ED25519 key).
  The CLI therefore authenticates via the agent first and only falls back to
  reading cfg.ssh.key directly.
- All nodes: aarch64, Raspberry Pi OS **Trixie** (Debian 13), kernel 6.18.x-rpi-v8,
  boot partition mounted at `/boot/firmware`, 32 GB SD cards (~32,026,656,768 B;
  003 reports 32,010,928,128 — cards vary slightly). Passwordless sudo on all
  four since the golden rollout.
- Pi 3 eth0 is USB (smsc95xx). Assumed built into the RPi kernel (=y); if the
  agent never sees eth0 on hardware, this assumption failed → blocker.
- The owner has physical access for disaster recovery and authorized reflashing
  and testing on all four nodes.

## Build machine (this Mac)

- macOS (darwin/arm64), go 1.27.0, `xz` and `zstd` CLIs in /opt/homebrew/bin,
  `file` available. Docker exists but MUST NOT be used by the tool.
- Repo: `~/Code/rasputin` (git, local only, no remote — never push).
- Mac LAN IP was 192.168.0.228 (DHCP; always auto-detect at runtime, never
  hardcode: pick the interface with the default route).

## Decisions already made by the owner

- Language Go; config from YAML; only official Raspberry Pi OS (Debian) supported.
- Provisioned user on nodes: `berry`, SSH key `~/.ssh/id_ed25519.pub`,
  passwordless sudo, password SSH auth off. **All four nodes now run as
  `berry`**; the CLI still tries SSH users in config order.
- Timezone `America/Sao_Paulo`, locale `en_US.UTF-8`.
- Packages: `podman curl htop vim git tmux`.
- Wi-Fi + Bluetooth disabled (config.txt: `dtoverlay=disable-wifi`,
  `dtoverlay=disable-bt`). cmdline.txt gains `cgroup_enable=memory cgroup_memory=1`.
- Builder/guinea node: rasputin001 (only one with passwordless sudo today).

## Vanilla image

- Source: `https://downloads.raspberrypi.com/raspios_lite_arm64_latest`
  (HTTP redirect to the latest `*-raspios-*-arm64-lite.img.xz`). Record the
  resolved URL + sha256 in `out/meta/vanilla.json`. Cache the decoded .img in
  `cache/` (gitignored).
- Image layout: MBR; p1 FAT32 boot (`bootfs`), p2 ext4 root (`rootfs`).
  On boot the stock image auto-expands rootfs to the whole card — `prepare`
  MUST remove that hook (T11) and let firstrun.sh grow rootfs to the
  configured cap instead, or captures become 32 GB streams. **Verified on
  the 2026-06-18 Trixie image (T09): the hook is now a bare `resize` token,
  not an `init=/usr/lib/raspberrypi-sys-mods/init_resize.sh` hook.** Both
  forms are stripped by bootfs.WithFirstrun.
- Verified image (T08): `2026-06-18-raspios-trixie-arm64-lite.img`,
  2,977,955,840 B decoded (sha256 e235fd24…c33a9), p1 starts at sector 8192,
  stock cmdline.txt is 110 B with `root=PARTUUID=041bba91-02`, stock
  config.txt is 1272 B and ends with an `[all]` section.
- The boot partition is mounted at `/boot/firmware` on Bookworm/Trixie, so
  the firstrun hook is `systemd.run=/boot/firmware/firstrun.sh` (PLAN.md's
  architecture section says `/boot/firstrun.sh`, which contradicts its own
  T10 paths; /boot/firmware is what the official Imager writes).
- go-diskfs quirk (T09): reading a FAT file to EOF returns the tail of the
  last cluster (stock config.txt: 1272 B on disk, 1536 B read back). Always
  bound the read by `Stat().Size()` — `bootfs.ReadFile` does.
- config.txt on Bookworm/Trixie has `auto_initramfs=1`; our explicit
  `initramfs recovery.gz followkernel` line replaces it (remove auto_initramfs).
- cmdline.txt is a single line; `root=PARTUUID=xxxxxxxx-02` — dd-cloning the
  golden image clones the MBR disk id, so PARTUUID stays valid on every node.

## Hard-won implementation facts (from the legacy prototype)

- An initramfs needs `/dev/console` (char 5:1, mode 0600) present in the cpio
  for early console output. mknod-free: emit the newc entry directly — header
  fields (all %08x hex after magic `070701`): ino=1, mode=0x2180, uid=0, gid=0,
  nlink=1, mtime=0, filesize=0, devmaj=0, devmin=0, rdevmaj=5, rdevmin=1,
  namesize=12 (`dev/console\0`), check=0; name NUL-padded so 110+namesize
  rounds up to a multiple of 4. Kernel accepts entries in any order.
- Boot flow facts: with an initramfs present the kernel ignores `root=` and
  runs `/init` as PID 1. Mount devtmpfs on /dev (proc, sysfs likewise) before
  anything. Log via writes to `/dev/kmsg` ("rasputin: ..." prefix).
- Device paths are fixed on Pi 3 + SD: disk `/dev/mmcblk0`, boot p1, root p2.
- switch_root equivalent in Go: mount p2 ro at /newroot → move /dev into it →
  chdir("/newroot") → mount(".", "/", MS_MOVE) → chroot(".") → chdir("/") →
  exec /sbin/init. Rootfs must be mounted ro so systemd can fsck+remount.
- Safety ordering (owner constraint): never open /dev/mmcblk0 for writing until
  an HTTP probe (HEAD, or GET with immediate close) of the image URL succeeds.
  Pre-write failures retry forever (backoff 2s→60s). Mid-write failures log to
  kmsg and retry the entire stream forever (RAM-resident, so not a brick).
  zstd frame checksums (klauspost: EncoderCRC on) are the integrity check.
- Flag semantics: dryrun beats capture beats reflash if several exist. Flag
  file's first line, if non-empty, overrides the baked default URL. Manual
  trigger `ssh node 'sudo touch /boot/firmware/reflash && sudo reboot'` must
  keep working (empty flag → baked default URL from YAML via -ldflags).
- Raspberry Pi OS ships `regenerate_ssh_host_keys`-style oneshot; empty
  /etc/machine-id regenerates automatically. If host keys do NOT regenerate on
  Trixie, the identity service must run `ssh-keygen -A` when keys are missing.

## Verification norms

- Every aarch64 artifact: parse with Go `debug/elf` — Machine==EM_AARCH64,
  no PT_INTERP (static). (`file` output is for humans; the Makefile check must
  be Go-based so it needs no external tools.)
- Unit tests run with `go test ./...` on darwin; packages with linux-only
  syscalls must build-tag them so the module still vets/tests on darwin
  (`GOOS=linux go build ./...` must also pass).
- Golden/vanilla image content checks are done by re-reading the FAT partition
  with go-diskfs and asserting file presence + content, never by mounting.


## Measured performance (hardware, 2026-08-29, after the speedup lanes)

Baselines in brackets are the pre-speedup measurements from T17-T21.

| operation | measured | was |
|---|---|---|
| `bake` (4 GiB cap) | **16m22s** | 22m30s |
| — of which capture | **4m9s** @ 17.25 MB/s over 4,294,967,296 B | 11m3s @ 12.96 MB/s over 8,589,934,592 B |
| capture, same-size comparison (8 GiB cap) | **7m11s @ 19.93 MB/s** | 11m3s @ 12.96 MB/s — **1.54x** |
| `flash <node>` solo | **5m40s** | 10m28s - 13m18s |
| `flash all` (2 flashed, 2 skipped) | **7m13s** wall; 7m13s / 5m37s per node | 10m29s - 12m48s for 2 |
| `flash` skip of a node already on the build | **0-1 s** | n/a (new) |
| `adopt <node>` | 45.8 s | 40-55 s |
| `dryrun -vanilla` | 2m10s, 2,977,955,840 B @ 40.8 MB/s decode | 2m23s @ 40.6 MB/s |
| `prepare` (full, warm cache) | 12.6-16.5 s | ~11-30 s |
| `status` (4 nodes) | ~2 s | 8 s |

**Current golden** (`out/meta/golden.json`): build `20260829T205033Z-307368`,
**1,177,967,475 B** compressed, **`card_used_bytes` 4,294,967,296**, sha256
`d5611d9bac11630797907f7ab21bb411b922a54866e313495125969682819848`.
Previous golden was 2,448,792,419 B over 8,589,934,592 B of card.

- **The rootfs cap is 4 GiB** (`rasputin.yaml` `image.rootfs_size_gb`) and it
  applies to the BAKED image only. Clones grow to the whole card on first boot
  via `rasputin-identity`, gated on the `/var/lib/rasputin/grow-rootfs` marker
  that seal creates immediately before capture. Measured: a clone comes up
  with ~29G usable, 25G free.
- **The cap is consumed at PREPARE time, not bake time.** Editing
  `rasputin.yaml` and baking without re-running `prepare` produces a golden at
  the old cap. Always: edit yaml -> `prepare` -> `rm out/vanilla-custom.img`
  -> bake.
- **Golden compressed size is NOT a stable property of the build.** Seal
  deliberately never zeroes free space, so a capture carries whatever stale
  bytes sit on the card; compressed size tracks the card's history. Measured
  proof: the 8 GiB golden (1,124,964,220 B) came out *smaller* than the 4 GiB
  one (1,177,967,475 B) because its extra 4 GiB was trimmed-to-zero. Never
  gate on golden size; gate on `card_used_bytes`, the decode verification and
  a real clone.
- `verifyImage` decodes the whole stream with CRCs on and requires the decoded
  length to equal the partition table's `UsedBytes`, so a truncated capture
  cannot become the golden.
