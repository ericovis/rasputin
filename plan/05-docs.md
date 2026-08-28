# Phase 5 — docs & wrap-up (T22)

## T22 — README, cleanup, final report

- `README.md` rewrite for the Go tool:
  - What it is; architecture sketch (agent-as-init, boot-partition-only
    customization, bake-on-a-Pi golden images, flag-file trigger protocol).
  - Quickstart: edit rasputin.yaml → `make build` → `rasputin prepare` →
    `rasputin adopt all` → `rasputin bake` → `rasputin flash all`.
  - Day-0 (virgin SD card, physical): flash `out/vanilla-custom.img` with
    Raspberry Pi Imager or `dd` (exact macOS diskutil+dd commands), boot,
    then the node is adoptable/bakeable remotely. Note: adopt is only for
    nodes already running an OS; virgin cards use this path.
  - Day-2 loop: change YAML → `prepare` → `bake` → `flash all`.
  - Manual trigger escape hatch: `ssh node 'echo URL | sudo tee
    /boot/firmware/reflash && sudo reboot'` with `rasputin serve` running.
  - Troubleshooting: node stuck in recovery (it retries forever; check
    server reachability, then power-cycle — card untouched pre-stream);
    reading reflash-dryrun.log; recovering a bad adopt (pull SD, remove
    initramfs line, or restore config.txt.pre-rasputin).
  - Security tradeoffs: host keys regenerate per flash (CLI records them in
    out/state.json), LAN-only HTTP, passwordless sudo requirement.
- Cleanup: `gofmt -l .` empty; `go vet ./...` clean; delete dead code and any
  temp debug subcommands (e.g. vanilla-fetch if hidden); ensure `.gitignore`
  matches reality (`git status` shows no image artifacts).
- Final PROGRESS entry: summary table of all phases, total wall time of a
  `flash all`, open blockers, and suggested next steps (e.g. rasputin003
  onboarding when it comes online, `sd` write command as future work,
  in-agent debug HTTP endpoint as future work).

VERIFY:
```
gofmt -l . | wc -l | grep -q '^ *0$'
go vet ./... && go test ./...
grep -q 'rasputin adopt' README.md && grep -q 'Day-0' README.md
git status --porcelain | grep -Ev '^\?\? (out/|cache/)' | wc -l   # only committed tree
```
