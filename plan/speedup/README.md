# Speedup plan — orchestrator overview

Produced 2026-08-29 against commit `9e31059` ("add CLAUDE.md with the non-obvious
constraints for future sessions"). Four implementation lanes, each written as a
fully self-contained spec that a Claude Opus 5 agent can execute with **zero
context beyond the spec file and the repo**. Every spec was drafted from the
real code, adversarially reviewed (anchor accuracy, CLAUDE.md trap compliance,
executability, lane isolation, technical correctness), revised, and then
cross-checked for integration conflicts. The rulings from that final
integration check appear as **ORCHESTRATOR AMENDMENTS** at the top of each lane
file — amendments override the spec body wherever they differ.

## Goal

| Metric | Baseline (hardware-measured) | Expected after all lanes |
|---|---|---|
| `flash <node>` | 10m28s – 13m18s | **~6–9 min** |
| `bake` | 22m30s | **~13–15 min** |
| — of which capture | 11m3s | ~3–4 min |
| golden.img.zst | 2,448,792,419 B (8,589,934,592 B decoded) | ~1.1–1.3 GB (~4.295 GiB decoded) |
| needless builder re-flash after every bake | 10–13 min | 0 (skipped) |

## The lanes

| Lane | Spec file | What it does | Isolated gain |
|---|---|---|---|
| A | `lane-A.md` | `internal/agent`: parallel zstd capture encode (A1) + overlapped fsync in the SD write path (A2) | capture 663 s → ~375–450 s; flash write +15–60 s faster |
| B | `lane-B.md` | `internal/cluster` + `cmd/rasputin`: `provisionPoll` 20s→5s (B1) + flash skips nodes already on the golden build, `--force` to override (B2) | ~8 s/bake; kills the 10–13 min post-bake builder re-flash |
| C | `lane-C.md` | `rasputin.yaml` + `internal/provision`: bake a 4 GiB golden; clones grow to full card on first boot, gated on a seal-created marker | flash write 540 s → ~270 s; capture bytes halved |
| D | `lane-D.md` | Hardware validation runbook (creates `plan/SPEEDUP.md`, then executes it) — **strictly serial, owner-gated, destructive** | validates everything; updates FACTS/PROGRESS/CLAUDE/README |

`plan/SPEEDUP.md` does not exist yet — Lane D creates it. Do not create it earlier.

## Execution model

Lanes A, B, C touch **disjoint file sets** (verified by the integration check,
amendments included) and can run as three parallel agents. Lane D runs strictly
after all three have merged and `make test` is green.

```
        ┌─────────┼─────────┐
        ▼         ▼         ▼
      Lane A    Lane B    Lane C          three parallel agents,
   internal/  cluster +  yaml + provision  disjoint files
      agent   cmd/rasputin  templates+tests
        └─────────┼─────────┘
   merge A → make test
   merge B → make test
   merge C → make build && make test      (C last: it changes what
        │                                  `prepare` renders, and
        ▼                                  `make build` regenerates out/)
      Lane D — STRICTLY SERIAL, OWNER-GATED HARDWARE
   preflights → (bake) → flash one node → flash all → doc updates
```

- **One agent per lane, never split a lane.** Each lane file states its
  exhaustive allowed file set; staying inside it is what makes the parallelism
  safe.
- Parallel execution: either three git worktrees on per-lane branches
  (`speedup/lane-a` etc.) merged into `main` in the order A → B → C with
  `make test` after every merge, or simply run the lanes sequentially on
  `main` — the file disjointness makes either safe. **No remote exists; never
  push** (CLAUDE.md ground rule; it binds every executor).
- Any A/B/C order works; A → B → C is recommended so the image-affecting lane
  lands last and `make build` runs once.
- Lane D must not start until A, B and C are all merged: its preflights look
  for artifacts the other lanes produce (A2's rate log lines, B2's skip line,
  C's grow logic in `identity.sh.tmpl` and the `/var/lib/rasputin/grow-rootfs`
  marker from seal) and stop if any is missing.

## Savings ledger — do not sum per-lane numbers

Each lane's spec states its gain **in isolation**. Combined gains are
bottleneck-max, not additive:

- **Capture**: A alone lifts the 12.96 MB/s encode bound until SD read binds
  (~6.5–7.5 min); C alone halves bytes (~5.5 min); **combined** ≈ 4.295 GiB at
  the ~20–23 MB/s SD read ceiling ≈ **3.1–3.6 min** (wire floor ~2 min).
- **Flash**: C cuts the ~540 s SD write to ~270 s; A2 raises the write rate on
  those bytes to ~17–18 MB/s (~240–250 s); A1 contributes **nothing** to flash
  (flash is SD-write-bound; decode was already proven at 35.4 MB/s). Plus the
  unchanged ~3–4 min boot/verify tail ⇒ **~6–9 min** total.
- **Bake**: ~13–15 min combined (reflash ~5–6 min, provision ~4 min,
  capture ~3–4 min, verify ~1 min).

Lane D's acceptance bands use the combined figures. `plan/FACTS.md`,
`CLAUDE.md` and `README.md` get **measured** values only, written by Lane D
after hardware validation — never these predictions.

## Cross-lane rulings (from the integration check)

1. **`ROOTFS_CAP_GB` test assertions must derive from config, not a literal**
   (Lane C amendment). `provision_test.go` renders against the real
   `../../rasputin.yaml`, and Lane D's two-bake path temporarily toggles
   `rootfs_size_gb` back to 8 mid-runbook; a `=4` literal would break
   `go test ./...` while hardware work is in flight.
2. **Docs belong to Lane D.** Lane C must not touch `CLAUDE.md`/`README.md`/
   `plan/*`; Lane D updates them (and `plan/FACTS.md`) with measured values
   after hardware validation. Lane D's CLAUDE.md open question is resolved
   affirmatively: edit it, no fallback note.
3. **Lane D's `rasputin.yaml` toggle-and-restore is approved.** The cap is
   config-driven end to end (`firstrun.sh.tmpl` `ROOTFS_CAP_GB={{.RootfsSizeGB}}`
   ← `provision.go:74`); every toggle must be followed by a re-`prepare`
   (`make build`) because the cap is consumed at render time.
4. **Lane B keeps `cmd/rasputin/main.go`** (usage-string edit; no other
   claimant), reuses `buildIDFrom` from `status.go` without editing `status.go`
   or `cluster.go`, hard-codes no golden byte sizes, and must guarantee bake's
   own builder-reflash never consults the B2 skip logic.
5. **Lane A needs no `!linux` stub** as designed (portable code references only
   the `RangeSyncer` interface; the concrete type lives in the linux-tagged
   file). If implementation drifts so that portable code references a
   linux-tagged symbol, add `internal/agent/syncrange_other.go`
   (`//go:build !linux`, `boot_other.go` pattern) rather than weakening tags.

## Non-negotiables for every executor

- Read `/Users/ericovis/Code/rasputin/CLAUDE.md` first; its traps all have
  regression tests and several sit adjacent to these edits.
- `go test ./...` green on darwin (no hardware) and
  `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` green — after every
  task, not just at the end.
- Lanes A–C never run `flash`/`bake` or touch the physical cluster. Lane D
  gates every destructive step on fresh, explicit owner confirmation naming
  the nodes at risk.
- No remote. Never push. `out/` and `cache/` stay uncommitted.
