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
