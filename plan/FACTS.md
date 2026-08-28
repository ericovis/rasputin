# Ground-truth facts (verified 2026-08-28)

Do not re-derive these; trust them unless a VERIFY step proves otherwise.
If you discover one is wrong, update it here and note it in PROGRESS.md.

## The cluster

4× Raspberry Pi 3 Model B, wired ethernet, headless, DHCP on 192.168.0.0/24.
SSH works today as user `ericovis` (the Mac's default user, key
`~/.ssh/id_ed25519`) to `rasputinNNN.local` (avahi/mDNS, active on all nodes).

| node        | MAC (eth0)          | last IP        | sudo -n | state |
|-------------|---------------------|----------------|---------|-------|
| rasputin001 | `b8:27:eb:01:02:03` | 192.168.0.74   | YES     | up; runs podman workloads (owner authorized wiping) |
| rasputin002 | `b8:27:eb:04:05:06` | 192.168.0.124  | NO      | up    |
| rasputin003 | `b8:27:eb:07:08:09` | —              | ?       | OFFLINE / unreachable |
| rasputin004 | `b8:27:eb:0a:0b:0c` | 192.168.0.190  | NO      | up    |

- MAC↔name above is the LIVE cluster state (verified over SSH). The owner once
  pasted a list implying 002/004 swapped; live state wins. 003's MAC comes from
  the owner (only MAC not seen live).
- All nodes: aarch64, Raspberry Pi OS **Trixie** (Debian 13), kernel 6.18.x-rpi-v8,
  boot partition mounted at `/boot/firmware`, 32 GB SD cards (~32,026,656,768 B),
  passwordless-sudo only on 001 (see BLOCKERS).
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
  passwordless sudo, password SSH auth off. (Current nodes use `ericovis`; the
  CLI must try SSH users in config order, e.g. [berry, ericovis].)
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
  On boot the stock image auto-expands rootfs to the whole card via an
  `init=` hook in cmdline.txt — `prepare` MUST remove that hook (T11) and let
  firstrun.sh grow rootfs to the configured cap instead, or captures become
  32 GB streams.
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
