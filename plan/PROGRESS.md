# Progress log (append-only)

- 2026-08-28 · plan · Plan created. Preflight survey done over SSH (results
  in FACTS.md): all nodes aarch64/Trixie//boot/firmware/32GB; live MAC↔name
  mapping recorded (differs from an earlier owner-pasted order — live state
  adopted); 003 offline; 002/004 lack passwordless sudo (see BLOCKERS).
  Legacy shell prototype (validated: static aarch64 busybox+zstd initramfs,
  crafted /dev/console cpio entry, init script signed off) moved to legacy/
  as the behavioral reference for the Go agent.

- 2026-08-28 · T01 · Module scaffold done: go.mod (module
  github.com/ericovis/rasputin, go 1.27) with all seven planned deps,
  .gitignore, Makefile (build/test/clean), rasputin.yaml (live config),
  internal/config (load, defaults, ~ expansion, MAC normalization,
  validation incl. duplicate-MAC and unknown-field rejection) with tests,
  and cmd/rasputin with stdlib subcommand dispatch (all stubs return
  "not implemented"). Learned: server.port needs a *int so an explicit 0
  ("random free port") is distinguishable from an absent key defaulting to
  8080. `make build` will fail until T04 adds cmd/mkinitramfs — intended.

- 2026-08-28 · T02 · internal/cpio: newc writer (WriteDir/WriteFile/
  WriteCharDev/Close) with 4-byte name+data padding, zero mtime for
  reproducible builds, lowercase %08x fields, sticky write errors. Tests:
  byte-exact golden for the dev/console entry from FACTS.md plus a
  round-trip through a minimal in-test newc reader (darwin cpio not used).

- 2026-08-28 · T03 · internal/kmsg (concurrency-safe multi-sink logger,
  every line prefixed "rasputin: ", errors from sinks swallowed so logging
  never aborts a reflash; Open() behind linux/!linux build tags) and
  internal/mbr (Parse/Read/UsedBytes, disk-ID exposed since dd-cloning it is
  what keeps root=PARTUUID valid, rejects GPT-protective and unsigned
  sectors). UsedBytes takes the max partition end, not the last entry.

- 2026-08-28 · T05 · Agent skeleton: internal/agent/mode.go (portable
  SelectMode with dryrun>capture>reflash priority, first-line URL override,
  ModeError when no URL is resolvable) + boot_linux.go (pseudo-FS mounts
  tolerating EBUSY, 10s device wait, ro flag read, WithBootRW helper,
  switch_root per FACTS, Reboot, eth0 MAC) + boot_other.go stubs so darwin
  still builds/vets/tests. cmd/agent/main.go refuses to run unless PID 1
  (or RASPUTIN_AGENT_TEST=1), recovers panics into a log loop, and never
  exits. SelectMode takes defaultURL as a second argument (the plan's
  one-argument signature cannot express the "no default" error case).
  Design concern about ModeError not booting recorded in BLOCKERS.md.

- 2026-08-28 · T04 · internal/initramfs.Build: cross-compiles ./cmd/agent
  (CGO off, linux/arm64, -trimpath, -s -w, -X main.defaultURL / main.version),
  verifies EM_AARCH64 + no PT_INTERP via debug/elf, warns above 12 MiB, packs
  dev/ + dev/console(c 5:1 0600) + init(0755) with internal/cpio and gzips at
  BestCompression. Driver is cmd/mkinitramfs (chosen over a CLI flag; the
  Makefile `build` target calls it). Added an exported cpio.Read/Find so the
  build can verify its own archive; the T02 test now uses it. Current sizes:
  agent 1.70 MB, recovery.gz 709 KB — well inside the RAM budget.

- 2026-08-28 · T06 · internal/agent/netup_linux.go: NetworkUp brings up lo
  and eth0 via netlink, waits for the link to appear (logging "driver
  missing?" every 10s), waits up to 30s for carrier (advisory), then DORA
  via nclient4 with AddrReplace + RouteReplace. Retries DHCP forever every
  3s; no renewal loop by design (a recovery session is minutes long and a
  renewal goroutine is another way to wedge PID 1). Note: `go get
  github.com/insomniacslk/dhcp/dhcpv4/nclient4` was needed separately —
  its linux-only transitive deps (mdlayher/packet, u-root/uio) are invisible
  to a darwin-only `go get`. Real validation is hardware T18.

- 2026-08-28 · T07 · Agent pipelines. internal/agent/pipeline.go (portable):
  Backoff 2s→60s, Client with injectable Sleep/Now, Probe (HEAD, GET fallback
  on 405), WaitProbe (0 = forever), Stream (zstd with checksum verification →
  Target, 4 MiB buffer, progress+Sync every 256 MiB), Upload (zstd POST of
  exactly N bytes through an io.Pipe), DryrunReport formatting. run.go: the
  Disk and System interfaces plus Reflash / Dryrun / Capture / RunMode.
  disk_linux.go binds them to /dev/mmcblk0 and the real reboot; boot_other.go
  stubs them for darwin. cmd/agent now runs the network modes. 29 unit tests
  cover the pipeline against httptest, including corrupted and truncated
  streams, and assert the safety invariant directly: with an unreachable
  server the card is never opened for writing.

- 2026-08-28 · T08 · internal/vanilla.Ensure: streams the stock image from
  the redirecting download endpoint, hashes the xz and the decoded image as
  they pass, decodes with ulikunitz/xz into cache/<name>.img via a temp file
  (so a failed download leaves nothing that could be mistaken for a good
  image), and records provenance in out/meta/vanilla.json. Re-runs are served
  from the cache unless the file is missing or its size no longer matches.
  Hidden CLI subcommand `vanilla-fetch` drives it. Fetched for real:
  2026-06-18-raspios-trixie-arm64-lite.img — 500 MiB xz → 2,977,955,840 B
  image, sha256 e235fd24…c33a9, in ~100s.

- 2026-08-28 · T09 · internal/bootfs. Image wrapper over go-diskfs
  (Open/ReadFile/WriteFile/Remove/Exists/List/Stat) plus pure patch functions
  (PatchConfigTxt, WithFirstrun/WithoutFirstrun, NodesConf). Verified against
  the real 2026-06-18 Trixie image: reads, large writes (720 KB), shrinking
  overwrites, deletes and re-open all round-trip correctly, so **the mtools
  fallback is NOT needed**. Two findings recorded in FACTS.md: go-diskfs
  over-reads FAT files to the end of the last cluster (bounded now by the
  directory-entry size), and this image's rootfs auto-expansion hook is a
  bare `resize` token rather than an `init=` hook. Stock config.txt and
  cmdline.txt committed as testdata fixtures. Deviation: the firstrun hook
  path is /boot/firmware/firstrun.sh, not PLAN.md's /boot/firstrun.sh —
  see FACTS.md; PLAN's own T10 steps use /boot/firmware paths.

- 2026-08-28 · T10 · internal/provision: five go:embed'd POSIX-sh/systemd
  templates rendered from the cluster config — firstrun.sh (grow rootfs to
  the cap with sfdisk+resize2fs, create the locked `berry` account with the
  authorized key and validated sudoers drop-in, sshd keys-only, timezone and
  locale, install+enable the identity and provision units, write
  /etc/rasputin-release, then strip the systemd.run triplet from cmdline.txt
  so it can never run twice), rasputin-identity (+unit: hostname from the
  eth0 MAC via nodes.conf, ssh-keygen -A when host keys are missing, never
  blocks the boot), rasputin-provision.service (apt with Restart=on-failure
  and a provisioned marker), and rasputin-seal (host keys, machine-id,
  journal, apt lists; deliberately does not reboot). Rendering uses
  missingkey=error. Tests assert the key lines and run `sh -n` over every
  rendered script.

- 2026-08-28 · T11 · `rasputin prepare` wires T04+T08+T09+T10 together:
  builds out/recovery.gz, copies the cached stock image to
  out/vanilla-custom.img, writes recovery.gz + nodes.conf + the five
  provisioning files + rasputin-build-id + the `ssh` marker onto the FAT boot
  partition, patches config.txt and cmdline.txt, then zstd-compresses
  (SpeedBetterCompression, CRC on) to out/vanilla-custom.img.zst and records
  everything in out/meta/prepare.json. `-initramfs-only` is the Makefile's
  build step. Real run: 2,977,955,840 B → 735,551,529 B (24.7%) in ~11s with
  a warm cache; build id 20260828T233619Z-40fd3e. The agent is now 6.5 MB
  (netlink + dhcp + zstd) and recovery.gz 2.7 MB — still well inside the RAM
  budget. TestImageContents (the no-hardware end-to-end gate) passes: /init
  is a static aarch64 ELF inside the cpio, dev/console is char 5:1, all four
  MACs are in nodes.conf, config.txt has no auto_initramfs, cmdline.txt has
  neither an init= hook nor `resize`, and firstrun.sh passes `sh -n`.

- 2026-08-28 · T12 · internal/server: binds the auto-detected LAN address
  (UDP-dial trick, refuses loopback) on the configured port, serves
  registered images under /i/<name> while counting bytes per client, and
  receives captures at POST /capture?id=. Progress is keyed by the node's
  MAC (header or query) and falls back to its IP. HEAD — the agent's
  pre-write probe — answers without creating a progress entry. Captures are
  single-flight per id, streamed to out/incoming-<id>.zst.tmp with a running
  sha256, and only fsync+renamed into place on a clean EOF, so a truncated
  upload can never be mistaken for a golden image; a repeat POST after
  success answers 200 again rather than making the node re-stream. 14 tests,
  race-clean.

- 2026-08-28 · T13 · Three packages. internal/state: atomic 0600
  out/state.json cache (last IP, SSH user, host key, hostname, build id);
  a corrupt file is treated as empty rather than fatal. internal/sshx:
  key-based dialer that tries cfg.ssh.users in order, Run/Sudo/Output/
  Push (via `sudo -n sh -c 'mkdir -p … && cat > … && chmod && sync'`, no
  sftp)/Fetch, and trust-on-first-use host keys pinned in the state cache —
  ForgetHostKey is what every reflash path must call, since nodes regenerate
  their keys by design. internal/nodes: Select (name | MAC | last-seen IP |
  all), Candidates (cache → mDNS → ARP), Connect — which **always verifies
  eth0's MAC after connecting and refuses a mismatched machine**, given this
  cluster's history of duplicate hostnames — WaitFor with a deadline-bounded
  poll, and RunPreflight (aarch64, /boot/firmware mounted, sudo -n, boot
  space ≥ 2× recovery.gz) whose sudo failure message prints the exact
  remedy. sshx is tested against a real in-process SSH server, so the
  user-fallback and host-key-pinning paths run a genuine handshake.

- 2026-08-28 · T14 · internal/cluster + the `adopt` and `dryrun` commands.
  Adopt (sequential by design — each node reboots) runs preflight, backs up
  config.txt to config.txt.pre-rasputin exactly once, pushes recovery.gz and
  nodes.conf, patches the node's config.txt with the same PatchConfigTxt the
  image build uses, syncs, then by default reboots and proves the node comes
  back with the initramfs line intact and no agent errors in dmesg.
  Dryrun starts the HTTP server, arms the reflash-dryrun flag with its URL,
  reboots, prints transfer progress every 10s while waiting, then reads
  reflash-dryrun.log and passes only on `result: OK` with the flag cleared
  (a lingering flag means the report is stale). RebootAndWait waits for the
  node to actually go down first, so a reboot that never happened cannot
  pass silently. `--help` now exits 0 instead of erroring.

- 2026-08-28 · T15 · `flash`, `bake`, `status` and `serve`. flash runs nodes
  in parallel, forgets each recorded host key before rebooting (the clone
  regenerates them), watches server progress, then verifies build id ==
  golden build id, hostname == node name and systemd running/degraded.
  bake drives the full builder sequence: reflash with the prepared stock
  image → wait for /var/lib/rasputin/provisioned (surfacing the provision
  journal on timeout) → rasputin-seal → capture flag → await the upload →
  **verify the captured image decodes end to end and its length matches its
  own partition table before it is allowed to become golden.img.zst** →
  write out/meta/golden.json → confirm the builder comes back healthy.
  status probes in parallel, read-only.

  Two live-cluster findings, both recorded in FACTS.md and BLOCKERS.md:
  1. **The owner's SSH key is passphrase protected.** sshx now authenticates
     through the SSH agent first and falls back to the key file, with an
     error that says `ssh-add` when neither works.
  2. **rasputin003 is not offline** — it is up at 192.168.0.222 with the
     right MAC, but its hostname is `rasputin002`, so mDNS never finds it.
     Only the ARP candidate reaches it, and it was being starved by a shared
     dial budget; each candidate address now gets its own timeout. The MAC
     check did exactly its job here: nothing mistook it for rasputin002.
  `rasputin status` against the live cluster now prints 4 rows, all four
  reachable and MAC-verified, and modifies nothing.

- 2026-08-28 · T16 · **Hardware gate PASSED. The hardware phase begins;
  rasputin001 will be wiped (owner pre-authorized, including its podman
  workloads), and the owner re-confirmed "go ahead" in session.**
  Eligibility (eligible = reachable + passwordless sudo):
  | node        | reachable | sudo -n | eligible |
  |-------------|-----------|---------|----------|
  | rasputin001 | yes (mDNS)          | YES | **yes** — builder/guinea |
  | rasputin002 | yes (mDNS)          | no  | no |
  | rasputin003 | yes (ARP, .0.222)   | no  | no |
  | rasputin004 | yes (mDNS)          | no  | no |
  rasputin001 alone is enough for T17–T20. T21 stays blocked on the owner's
  sudo command for the other three (see BLOCKERS). rasputin003 is NOT
  offline — that blocker was wrong and is now marked resolved.
  out/vanilla-custom.img.zst rebuilt today (735,551,527 B) and the T11
  image-content gate re-run and passing.

- 2026-08-28 · T17 · **HW: adopted rasputin001 — the Go agent ran as PID 1 on
  real hardware for the first time and handed off correctly.** `adopt
  rasputin001` took **53 s** end to end including the reboot. Verified on the
  node: uptime reset to 0 min (it really rebooted), `dmesg` shows
  `rasputin: recovery agent 20260828T235727Z-2d5642 starting (pid 1)` at
  2.28 s and `rasputin: mode=normal mac=unknown url=` at 2.58 s, then systemd
  took over at 4.69 s; config.txt carries the managed block with the
  initramfs line and both dtoverlays; auto_initramfs is gone;
  config.txt.pre-rasputin (1272 B, the pristine stock file) exists;
  recovery.gz (2,702,889 B) and nodes.conf (165 B) are installed;
  `systemctl is-system-running` = running with no failed units.
  Observation: the early log line says `mac=unknown` because at 2.5 s in the
  initramfs the eth0 driver has not probed yet and /sys/class/net/eth0 does
  not exist. Cosmetic only — the network modes read the MAC after
  NetworkUp(), so capture/progress attribution is unaffected.

- 2026-08-28 · T18 · **HW: dryrun on rasputin001 PASSED — the whole recovery
  chain works on hardware.** `dryrun rasputin001` took **2m23s** end to end.
  The node's own report: `result: OK attempt=1 2977955840 bytes in 1m13s
  (40.6 MB/s)` — i.e. the full 2.98 GB decoded, comfortably above the 2.5 GB
  plausibility floor, with the zstd frame checksums verified. Server side:
  735,551,527 B served to MAC b8:27:eb:01:02:03 at a steady **~10 MB/s
  (≈80 Mbit)**, which is about what a Pi 3's USB ethernet gives. This proves
  on real hardware: the smsc95xx driver is built into the RPi kernel (the
  FACTS assumption held), netlink link-up works, DHCP works, HTTP streaming
  works, klauspost zstd decode works, and the agent cleared its flag and
  rebooted into the normal system. The SD card was never opened for writing.
  Note: the node came back via the ARP candidate (192.168.0.74) because mDNS
  had not re-announced yet — the multi-candidate resolver earned its keep.

- 2026-08-28 · T19 · **First bake attempt FAILED on a real bug, now fixed.**
  The builder reflashed, provisioned (podman 5.4.2 etc, finished 21:19:10)
  and grew its rootfs to 7.4 GB correctly — but the CLI then sat in
  waitProvisioned for 22 minutes without ever reconnecting.
  Cause: `flash`/`bake` cleared the pinned SSH host key *before* rebooting,
  but `waitGone` polls the node with Reachable() while it is still up, and
  the first of those handshakes re-pinned the very key that was about to be
  destroyed. After the reflash the node presented a new key and every
  connection was refused as an impersonation (recorded …BKV16gMF vs actual
  …BNk9rRzz). Fix: RebootAndWait now takes RebootOptions{ReplacesSystem} and
  drops the pin *after* the node is confirmed down, where nothing can re-pin
  it; the capture reboot in bake does the same, since sealing deletes the
  host keys. Regression test TestRebootReplacingSystemClearsTheStaleHostKey
  reproduces the exact failure (it fails with the old ordering, reporting
  "host key for rasputin001 changed") and passes with the fix.
  Lesson worth keeping: any "wait for it to go away" poll is itself a client,
  and clearing trust before the thing you distrust has actually gone is
  useless.

- 2026-08-28 · T19 · **Second bake attempt: the golden image WAS produced and
  verified, but the image itself had a defect. Two more real bugs found.**
  Timings (bake started 21:45:38): reflash+firstrun+reboot **5m35s**,
  provisioning **~4m**, seal seconds, capture **10m56s** — 2,440,062,750 B
  compressed from exactly 8,589,934,592 B of card (the 8 GB rootfs cap held
  precisely). The captured image passed end-to-end zstd decode and its length
  matched its own partition table. The reconnect-after-reflash worked, which
  is the host-key fix proving itself on hardware.
  Bug 3 (CLI): verifyClone sampled `systemctl is-system-running` the instant
  SSH answered and rejected the node for reporting "starting" — but sshd is
  up long before systemd finishes booting. Now waitSystemSettled() polls for
  up to 3 min, accepting running/degraded and tolerating
  starting/initializing/empty. Regression tests added.
  Bug 4 (image, the serious one): `userconfig.service` — Raspberry Pi OS's
  interactive first-boot user dialog — is `Type=oneshot` with
  `StandardInput=tty` on /dev/tty8, `Restart=on-failure`, and **no timeout**.
  On a headless node nobody ever answers it, so it blocks multi-user.target
  forever: systemd never reaches "running" and everything ordered after that
  target, including our own rasputin-provision.service, never starts.
  firstrun.sh now disables and masks it, drops the tty1 autologin drop-in,
  re-enables a normal getty, and removes the sshpwd nag banner.
  Bug 5 (mine, self-inflicted): seal was deleting
  /var/lib/rasputin/provisioned "so clones re-check their packages" — which
  would make every cloned node re-run apt on first boot, needing the internet
  to boot cleanly and defeating the point of a baked image. The marker now
  stays; a test asserts it.
  Also fixed: /etc/rasputin-release recorded `baked_at` from `date` on the
  node, but a Pi has no RTC and firstrun runs before NTP syncs, so it read
  the image's fake-hwclock date (2026-06-17, months stale). The build host's
  timestamp is now templated in as `prepared_at`, with `first_boot_at` kept
  separately for what the node itself observed.

- 2026-08-28 · T19 · **HW: golden image baked — PASSED.** Third attempt, with
  all four fixes in: `bake` completed in **22m55s** with exit 0 and the
  builder came back healthy on its own.
  Phase timings: reflash+firstrun ~6m, provisioning ~4m, seal seconds,
  capture **11m3s**, builder back + verified ~1m.
  Artifact: out/golden.img.zst, 2,453,921,771 B compressed from
  8,589,934,592 B of card (8 GiB exactly — the rootfs cap is precise),
  sha256 e9167ae9…61efe, build 20260829T011232Z-20880e.
  Full T19 checklist on the builder, all green: hostname `rasputin001` (set
  by the identity service from the MAC); **`systemctl is-system-running` =
  running** (this is the userconfig fix — it was stuck at "starting" forever
  before); build_id matches golden.json; podman 5.4.2 runs; timezone
  America/Sao_Paulo; `sudo -n` works as `berry`; userconfig masked;
  provisioned marker present; 0 failed units; all six packages
  (podman curl htop vim git tmux) on PATH; recovery.gz (2,702,887 B) present
  on the boot partition **inside the golden image**, so clones stay
  reflashable; `ericovis` no longer exists; password SSH auth refused.
  Cosmetic leftover for T22: sshd still shows the stock "SSH may not work
  until a valid user has been set up" banner (it comes from sshd's
  `Banner /run/sshwarn`, not the profile script firstrun removes). It goes to
  stderr, so it does not affect the CLI's stdout parsing.

- 2026-08-28 · T20 · **First flash-from-golden FAILED and exposed the worst
  bug yet: the golden image contained its own capture flag.**
  The flash itself worked perfectly — 2,453,921,771 B served to
  b8:27:eb:01:02:03 at ~4.3 MB/s, written and booted. But the node then came
  up in the recovery agent trying to POST its card back, 88 times, because
  /boot/firmware/capture was inside the image.
  Cause (bug 6): the agent removed the capture flag *after* streaming the
  card — but a capture streams the boot partition too, so the flag was still
  on disk while being read, and got baked into the golden image. Every node
  flashed from it would wake up believing it had been told to capture: a
  self-replicating trap. Fix: clear the flag *before* reading the card, and
  make failure to clear it fatal — producing no image is better than
  producing a poisoned one. Regression test
  TestCaptureClearsItsFlagBeforeReadingTheCard watches the boot partition at
  the exact moment the card is read; it fails against the old ordering.
  Recovery: no physical access needed. A throwaway HTTP server on
  192.168.0.228:8080 accepted the node's capture POST and discarded the body,
  so the agent got its 200, cleared its own flag and rebooted normally —
  which is the retry-forever design working exactly as intended.
  The poisoned golden.img.zst was deleted; re-bake required (the fixed agent
  also has to be baked in, since recovery.gz ships inside the image).

- 2026-08-29 · T20 · **HW: flash-from-golden PASSED — the production loop is
  proven and golden images are self-sustaining.** Re-baked first with the
  capture-flag fix (build 20260829T021418Z-66b2f9, 22m30s, 2,448,792,419 B).
  | run | result | duration |
  |-----|--------|----------|
  | flash rasputin001 (1st) | PASS | **13m7s** |
  | flash rasputin001 (2nd, repeatability) | PASS | **12m33s** |
  | dryrun rasputin001 (recovery survived the clone) | PASS | ~5m |
  Each flash: ~9m downloading 2,335 MiB at ~4.3 MB/s, then SD write + boot.
  Both flashes verified build_id == golden build_id, hostname == rasputin001
  (identity service, from the MAC) and systemd running/degraded.
  The dryrun ran *on a node flashed from golden* and reported
  `result: OK attempt=1 8589934592 bytes in 4m2s (35.4 MB/s)` — the full
  8 GiB decoded and checksum-verified, which proves recovery.gz and the
  config.txt hook are inside the golden image and keep working after a
  clone. No capture POSTs this time: the flag fix holds.

- 2026-08-29 · T21 · **BLOCKED — not a software problem.** rasputin002,
  rasputin003 and rasputin004 still refuse `sudo -n true` (re-checked
  00:26). Remote adoption needs passwordless sudo to write the boot
  partition, and granting it requires typing the account password on each
  node, which cannot be automated from here. The exact three commands are in
  BLOCKERS.md along with the follow-up adopt/flash sequence. Everything the
  blocked work depends on is already proven on rasputin001: adopt, dryrun,
  bake and flash all pass, and flash is verified repeatable. The parallel
  multi-node flash exercise also has to wait, since only one node is
  currently eligible.

- 2026-08-29 · T22 · README rewritten for the Go tool (architecture, quickstart,
  Day-0 physical write with the macOS `diskutil`/`dd` commands, Day-2 loop,
  the manual flag-file escape hatch, troubleshooting, and an explicit
  security trade-offs section covering per-flash host keys, MAC-based node
  verification, and unauthenticated LAN-only HTTP). Cleanup: removed the
  now-redundant `cmd/mkinitramfs` and the hidden `vanilla-fetch` subcommand
  (the Makefile uses `prepare -initramfs-only`, and `prepare` fetches the
  stock image itself), plus the unused `hidden` command field. gofmt clean,
  `go vet ./...` clean, all tests pass, no image artifacts outside out/ and
  cache/.

## Final summary

**Status: 21 of 22 tasks done. T21 blocked on an owner action, not on code.**

| phase | tasks | outcome |
|-------|-------|---------|
| 0 foundation | T01–T04 | done — module, config, cpio, kmsg, MBR, initramfs assembler |
| 1 agent | T05–T07 | done — boot/switch_root, netlink+DHCP, reflash/dryrun/capture |
| 2 image | T08–T11 | done — stock fetch, FAT32 editing, provisioning templates, `prepare` |
| 3 orchestration | T12–T15 | done — HTTP server, SSH/resolution/preflight, all six commands |
| 4 hardware | T16–T20 | done on rasputin001; **T21 blocked** (no passwordless sudo on 002/003/004) |
| 5 docs | T22 | done |

**Measured on real hardware (Raspberry Pi 3, 100 Mbit LAN):**

| operation | duration | notes |
|-----------|----------|-------|
| `adopt rasputin001` | **53 s** | includes a full reboot and verification |
| `dryrun rasputin001` | **2m23s** | 2.98 GB decoded at 40.6 MB/s |
| `bake` | **22m30s** | reflash ~6m, provision ~4m, capture 11m3s |
| `flash rasputin001` | **13m7s** / **12m33s** | two runs, ~9m of it downloading at ~4.3 MB/s |
| `status` (4 nodes) | **8 s** | parallel, read-only |

A `flash all` across four nodes should take about the same wall time as one
node — they run in parallel and the server is not the bottleneck; the SD
write is.

**Artifacts:** `out/golden.img.zst` — build `20260829T021418Z-66b2f9`,
2,448,792,419 B compressed from 8,589,934,592 B of card, sha256
`00c2b15a…c53f5`, baked from `2026-06-18-raspios-trixie-arm64-lite.img`.

**Six bugs were found and fixed, each with a regression test that fails
against the old code.** Five were only findable on hardware:
1. *(T15)* the owner's SSH key is passphrase protected — added SSH agent auth.
2. *(T15)* a shared dial budget starved the ARP candidate, hiding rasputin003.
3. *(T19)* the pinned host key was cleared before the reboot, so the "wait for
   it to go down" polling re-pinned the key that was about to be destroyed.
4. *(T19)* the health check sampled `systemctl is-system-running` before
   systemd had finished booting.
5. *(T19)* `userconfig.service`, Raspberry Pi OS's interactive first-boot
   dialog, blocks multi-user.target forever on a headless node.
6. *(T20)* the capture flag was cleared *after* the card was read, so it was
   baked into the golden image and every clone tried to capture itself.

**Open blockers:** rasputin002/003/004 need one `sudoers.d` command each from
the owner (exact commands and the follow-up adopt/flash sequence are in
BLOCKERS.md). rasputin003 is up and healthy but still answers to the
duplicate hostname `rasputin002`; flashing it fixes that permanently.

**Suggested next steps:**
- Run the T21 sequence once sudo is granted; it needs no further decisions.
- The parallel multi-node flash path is written and unit-tested but has never
  run against two real nodes at once.
- Zeroing free space before capture would shrink the golden image well below
  2.4 GB and cut flash time, at the cost of writing ~5 GB to the builder's
  card each bake. Deliberately not done.
- The stock `Banner /run/sshwarn` nag survives on flashed nodes (cosmetic,
  stderr only, does not affect the CLI's parsing).
- An in-agent debug HTTP endpoint would make a node stuck in recovery
  inspectable without the console.

- 2026-08-29 · post-plan · Two owner-requested changes.
  **1. `ssh.sudo` config parameter.** `passwordless` (default) keeps the old
  behaviour: require NOPASSWD and fail fast. `password` falls back to
  `sudo -S` for nodes that were never granted it. The password itself is
  deliberately NOT a config field — rasputin.yaml is committed, and a
  password in git is a password published — so it comes from
  $RASPUTIN_SUDO_PASSWORD or a no-echo prompt. A test asserts that a
  `sudo_password:` key in the YAML is *rejected*, so the decision cannot be
  quietly undone later.
  Implementation notes: the client probes `sudo -n true` once per connection
  and caches the answer. The password path uses `sudo -k -S -p ''` — `-k` is
  load-bearing, not decoration: it discards any cached credential so sudo
  ALWAYS consumes exactly one line of stdin. Without it a cached timestamp
  would make sudo skip the read, and for a file push the password line would
  be written into the file. TestPushWithSudoPasswordKeepsThePasswordOutOfThe-
  File pins that. Preflight's check is now just "sudo" (either path counts)
  and its hint adapts: a rejected password is told to check the env var, not
  lectured about a config setting already in effect.
  Verified against live hardware: `adopt rasputin002` with a deliberately
  wrong password reaches the node, has the password rejected, and stops at
  preflight without modifying anything.
  **2. known_hosts cleanup after a reflash.** A successful `flash` or `bake`
  now runs `ssh-keygen -R` on the build host for the node's name, its mDNS
  name and the address it was reached on, so the operator's own `ssh` keeps
  working instead of hitting REMOTE HOST IDENTIFICATION HAS CHANGED.
  Entirely best effort — a missing known_hosts or a missing ssh-keygen never
  turns a successful flash into a failure — and ssh-keygen writes its own
  .old backup.
  Note: rasputin002/003/004 still refuse `sudo -n true` as of 2026-08-29
  05:0x, so the owner's sudoers grant did not take effect. T21 can now
  proceed either by fixing that or by running with ssh.sudo: password.

- 2026-08-29 · T21 · **HW: the whole cluster is on the golden image. DONE.**
  The owner's sudoers grant took effect at ~05:10; all three remaining nodes
  then passed `sudo -n true`.
  | node | adopt | flash | notes |
  |------|-------|-------|-------|
  | rasputin002 | PASS | **13m18s** | first non-builder clone |
  | rasputin003 | PASS | **10m28s** | **duplicate hostname fixed** |
  | rasputin004 | PASS | **11m4s**  | |
  | rasputin002 + rasputin004 in parallel | — | **12m48s / 10m29s** | concurrent |
  **rasputin003's duplicate hostname is gone.** It had answered to
  `rasputin002` for as long as anyone knew, which is why `rasputin003.local`
  never resolved and why it looked offline during planning. After the flash
  it announces itself as `rasputin003.local` over mDNS with hostname
  `rasputin003` — applied purely from its MAC by the baked-in identity
  service, with no per-node image and nobody touching the hardware.
  **The parallel flash is the headline number:** two nodes rebuilt at once,
  each pulling at 4.5 MB/s — the *same* rate as a single node alone. The
  server is not the bottleneck; the SD card write is. So `flash all` across
  four nodes costs about what one node costs, ~13 minutes.
  Final state: all four nodes on build 20260829T021418Z-66b2f9, all
  provisioned, all reachable as `berry`, each with the correct per-MAC
  hostname.
