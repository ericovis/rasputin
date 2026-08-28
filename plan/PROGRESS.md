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
