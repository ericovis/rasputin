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
