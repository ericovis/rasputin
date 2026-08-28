# Phase 2 — image preparation (T08–T11)

## T08 — vanilla image download + xz + cache [P]A

`internal/vanilla`: `Ensure(cfg) (imgPath string, meta Meta, err error)`.
- GET cfg.image.source_url following redirects; final URL names the version.
- Stream-decode xz (ulikunitz/xz) to `cache/<basename>.img.tmp`, rename when
  complete; sha256 both compressed and decoded; write `out/meta/vanilla.json`
  {resolved_url, sha256_xz, sha256_img, bytes, fetched_at}.
- Skip download when cache file + meta exist and sizes match. ~0.5–1 GB xz,
  ~2.7 GB img — stream, never hold in memory. Progress line every 64 MiB.

VERIFY (network; run once, keep cache):
```
go test ./internal/vanilla/ -run TestUnit        # unit parts (paths, skip logic) with a tiny fake xz fixture
go run ./cmd/rasputin vanilla-fetch              # temp hidden subcommand or small test main — must produce cache/*.img and out/meta/vanilla.json
ls -l cache/*.img out/meta/vanilla.json
```

## T09 — FAT32 boot-partition editor `internal/bootfs`

Wrap go-diskfs: open .img (diskfs.Open, partition 1, FilesystemGet), then:
`ReadFile(name)`, `WriteFile(name, data)` (overwrite = delete+create if the
lib needs it), `Remove(name)`, `List()`. All paths are within the FAT root.
Add config helpers used by T11:
- `PatchConfigTxt(data)`: drop any `auto_initramfs=` line, append (idempotent)
  `initramfs recovery.gz followkernel`, `dtoverlay=disable-wifi`,
  `dtoverlay=disable-bt` under a `# rasputin` marker.
- `PatchCmdlineTxt(data)`: single line; remove `init=...firstboot|init_resize`
  token if present; ensure tokens `cgroup_enable=memory cgroup_memory=1`;
  add/remove the `systemd.run=/boot/firstrun.sh
  systemd.run_success_action=reboot systemd.unit=kernel-command-line.target`
  triplet (two funcs: WithFirstrun / WithoutFirstrun).
Patch functions are pure ([]byte→[]byte), unit-tested against a captured
sample of stock Trixie config.txt/cmdline.txt (get real ones in T11 and check
the fixtures in). If go-diskfs cannot reliably write this FAT32 (test T09's
round-trip!), STOP and record in BLOCKERS: fallback is shelling to `mtools`
(brew install mtools) — do not implement the fallback unless needed.

VERIFY:
```
go test ./internal/bootfs/ -v | grep -q PASS
# includes: create small FAT32 image via go-diskfs, write/read/overwrite files, re-open and re-read
```

## T10 — firstrun.sh / identity / provision templates [P]A

`internal/provision`: Go text/template rendering from config. Templates in
`internal/provision/templates/*.tmpl`, embedded via go:embed. All POSIX sh.
All log to /dev/kmsg with `rasputin:` prefix AND to /boot/firmware/rasputin-firstrun.log.

1. `firstrun.sh.tmpl` — runs once as root from boot partition (systemd.run),
   rootfs mounted rw. Steps, each idempotent, `set -eu`:
   - resize: grow p2 with sfdisk to min(rootfs_size_gb, card size), resize2fs.
   - user {{.User}}: useradd -m -s /bin/bash, lock password (`passwd -l`),
     install authorized_keys (0600, owned), `echo '{{.User}} ALL=(ALL)
     NOPASSWD:ALL' > /etc/sudoers.d/010-rasputin` (0440, visudo -c check).
   - sshd: /etc/ssh/sshd_config.d/rasputin.conf → PasswordAuthentication no,
     KbdInteractiveAuthentication no. Do NOT restart sshd here (reboots anyway).
   - timezone: ln -sf zoneinfo, echo to /etc/timezone. locale: enable in
     /etc/locale.gen, locale-gen, update-locale LANG=.
   - install /usr/local/sbin/rasputin-identity + rasputin-identity.service
     (enable), /usr/local/sbin/rasputin-seal, rasputin-provision.service
     (enable). Write /etc/rasputin-release: `build_id=... base=... baked_at=...`
     (build_id read from /boot/firmware/rasputin-build-id, written by prepare).
   - rewrite /boot/firmware/cmdline.txt WITHOUT the systemd.run triplet
     (hardcode the same removal sed as PatchCmdlineTxt does) — firstrun must
     never run twice. Then exit 0 (systemd.run_success_action=reboot reboots).
2. `identity.sh.tmpl` → /usr/local/sbin/rasputin-identity, oneshot service
   (DefaultDependencies=no, After=local-fs.target, Before=network-pre.target
   sshd avahi-daemon, WantedBy=sysinit.target): read eth0 MAC, look up
   /boot/firmware/nodes.conf (`mac<TAB>name` lines), if hostname differs:
   write /etc/hostname, fix 127.0.1.1 line in /etc/hosts, `hostname <name>`.
   If /etc/ssh/ssh_host_ed25519_key missing: `ssh-keygen -A`. Unknown MAC:
   hostname rasputin-unknown-<last6>, log loudly, exit 0 (must not block boot).
3. `provision.service.tmpl`: oneshot, After=network-online.target, Wants=;
   `apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y
   {{.Packages}}`; retries: Restart=on-failure, RestartSec=60; on success
   touch /var/lib/rasputin/provisioned and `systemctl disable
   rasputin-provision.service`.
4. `seal.sh.tmpl` → /usr/local/sbin/rasputin-seal: rm -f /etc/ssh/ssh_host_*,
   truncate -s0 /etc/machine-id, rm -f /var/lib/dbus/machine-id (symlink is
   fine to leave), `journalctl --rotate --vacuum-time=1s || true`. Does NOT
   reboot (CLI orchestrates).

Unit tests: render with the repo's rasputin.yaml, assert key lines present,
run `sh -n` on rendered firstrun/identity/seal (darwin sh is fine for -n).

VERIFY:
```
go test ./internal/provision/ -v | grep -q PASS
```

## T11 — `prepare` command

Wire T04+T08+T09+T10 into `rasputin prepare`:
1. Ensure vanilla (T08). Copy cache img → `out/vanilla-custom.img` (sparse-
   aware copy not needed; plain copy fine).
2. Build recovery.gz (T04) → out/ and into image.
3. bootfs edits on the copy: write recovery.gz, nodes.conf (from YAML nodes,
   `mac<TAB>name`), firstrun.sh, rasputin-build-id (new ULID or
   `time+rand` id, also into out/meta/prepare.json), `ssh` empty file,
   patched config.txt, patched cmdline.txt (WithFirstrun).
   First time: extract stock config.txt+cmdline.txt into
   `internal/bootfs/testdata/` as fixtures and commit them (see T09).
4. zstd-compress (klauspost, level 9-ish/SpeedBetterCompression, CRC on) →
   `out/vanilla-custom.img.zst` + sha256 in out/meta/prepare.json.
5. `--initramfs-only` flag: stop after step 2 (used by Makefile).

VERIFY:
```
go run ./cmd/rasputin prepare
go test ./internal/prepare/ -run TestImageContents -v
#   ^ opens out/vanilla-custom.img with go-diskfs and asserts: recovery.gz
#     present & gunzips to cpio containing /init (aarch64 static ELF, use T02
#     reader + debug/elf); nodes.conf has all 4 MACs; config.txt has initramfs
#     line + both dtoverlays and no auto_initramfs; cmdline.txt has cgroup
#     tokens + systemd.run + no init=; firstrun.sh passes `sh -n`; ssh file
#     exists. This test is the no-hardware end-to-end gate.
ls -lh out/vanilla-custom.img.zst
```
