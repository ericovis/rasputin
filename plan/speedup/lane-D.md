> **ORCHESTRATOR AMENDMENTS (post-integration review — these override the spec body below where they differ):**
>
> 1. **Ownership rulings — both open questions resolved affirmatively:** Lane D OWNS the execution-time edits to `CLAUDE.md` and `README.md` (edit them directly; no PROGRESS.md fallback note). The temporary `rasputin.yaml` `rootfs_size_gb` toggle-and-restore on the two-bake path is APPROVED: the cap is config-driven end to end, Lane C's tests derive their assertions from the config (so `go test ./...` stays green while toggled), and every toggle must be followed by re-`prepare` (`make build`) because the cap is consumed at render time.
> 2. **Acceptance bands use combined-bottleneck figures, never per-lane sums:** bake ~13–15 min (capture ~3–4 min: ≈4.295 GiB at the 20–23 MB/s SD-read ceiling; wire floor ~2 min), flash ~6–9 min, golden ≈1.1–1.3 GB compressed / decoded size read from the image's own partition table. Write only MEASURED values into `plan/FACTS.md`, `CLAUDE.md`, `README.md`.
> 3. **Lane C's artifacts for your preflights:** grow logic lives inside `identity.sh.tmpl` (no new template file); marker is `/var/lib/rasputin/grow-rootfs`, created by seal immediately before capture, deleted by the identity script on successful grow.
> 4. **B2 interaction:** time the flash on the first (non-skipped) pass; treat B2's skip line as the expected outcome only on a deliberate second pass; verify bake's own builder reflash never consults the skip logic. A skipped node proves nothing about flash speed.
> 5. **Do not start until lanes A, B, C are all merged** and `make test` is green on the merged tree; your preflights stop otherwise, before anything destructive.

# Lane D — hardware validation runbook for the speedup lanes (spec, rev 2)

## What this lane is

Lane D produces **one new file, `plan/SPEEDUP.md`** — a hardware validation runbook — and then (owner permitting, in the same session or a later one) **executes** it against the real 4-node Pi cluster, appending results to `plan/PROGRESS.md` and correcting `plan/FACTS.md`. **This lane changes no Go code.** It runs strictly AFTER lanes A (agent capture-encode concurrency + rate logging "A2"), B (orchestration, incl. "B2" builder-skip in `flash`), and C (rootfs cap 4 GiB + clone grow-to-full-card) have merged to `main` and `make test` is green.

Everything here was re-verified against the working tree at commit `9e31059` ("add CLAUDE.md with the non-obvious constraints for future sessions", branch `main`) on 2026-08-29. Every file:line anchor below was read directly; if a lane has moved a line, re-find it by the quoted snippet, not the number.

## Files this lane touches (exhaustive)

| file | action |
|---|---|
| `plan/SPEEDUP.md` | **create** (Task D1) — the runbook, then annotated with measured results during execution |
| `plan/PROGRESS.md` | **append** (Task D3, execution time only) |
| `plan/FACTS.md` | **edit** (Task D3, execution time only) |

Two more files are edited at execution time **only because the task sequence demands it and execution is serial/post-merge** — they are flagged as cross-lane in `open_questions` and the executor must confirm before touching them: `CLAUDE.md` (duration + build-id lines, step 6) and `rasputin.yaml` (temporary `rootfs_size_gb` toggle for the two-bake path, steps 2 and 4 — Lane C owns this file at merge time; Lane D only toggles one value and restores it). **Nothing in `out/` or `cache/` is ever committed** (both are gitignored; keep it that way).

## Ground rules the executor must hold at all times

From `CLAUDE.md` (repo root — read it first):

1. **No git remote. Never push.** Commit freely at the end.
2. **Every bake/flash is destructive (~13 min flash, ~23 min bake baseline) and wipes a node. Confirm with the owner before each one.** The runbook below encodes this as literal `OWNER GATE` lines. Do not proceed past a gate without an explicit "yes" from the owner *in the session*, naming every node the gate lists. An earlier blanket authorization does not carry forward past a failure. **A gate must name every node the command could conceivably wipe — never rely on unproven skip logic to keep a node out of a gate's scope.**
3. This phase is **strictly serial**: one physical cluster, one destructive operation at a time (step 5's parallel flash is one operation).
4. `bake` needs free disk on `/` (it has run out mid-capture before). Budget is computed in P6 below; `out/vanilla-custom.img` (2.9 GB, regenerable) is the first thing to delete.
5. **Timestamps for the log come from the Mac, never from a node** — a Pi has no RTC (`date` on a node is meaningless before NTP sync).

## Verified facts the runbook relies on (with anchors)

The executor does not need to re-derive these, but must re-verify the anchors still hold post-merge before running anything destructive.

**F1 — bake replaces the golden image atomically, but only mechanically-verified.** The capture is staged three times before it becomes golden:
- `internal/server/server.go:309-311`: `func (s *Server) StagePath(id string) string { return filepath.Join(s.opts.OutDir, "incoming-"+id+".zst.tmp") }` — upload streams to `out/incoming-<BuildID>.zst.tmp`.
- `internal/server/capture.go:121-125`: `stage := s.StagePath(id)` … `res.Err = os.Rename(stage, c.destPath)` — renamed on clean EOF + fsync only.
- `internal/cluster/bake.go:81`: `stagedPath := GoldenImage.Path + ".incoming"` — so `destPath` is `out/golden.img.zst.incoming`.
- `internal/cluster/bake.go:113-118`: `used, err := verifyImage(stagedPath)` … `if err := os.Rename(stagedPath, GoldenImage.Path)` — **`out/golden.img.zst` is only overwritten after the full zstd decode + partition-table check passes.**
- `internal/cluster/bake.go:130`: `if err := WriteGoldenMeta(meta)` — `out/meta/golden.json` is overwritten immediately after.

Consequence: an aborted or failed bake preserves the old golden. **But** a capture that is mechanically valid yet behaviorally poisoned (exactly what happened in T20, when the capture flag got baked in) sails through `verifyImage` and destroys the only working golden. Hence the mandatory backup in P5.

**F2 — a builder that dies mid-capture is not bricked; a power cycle boots the intact pre-capture system.** Verified in `internal/agent/run.go:159-174`: in `ModeCapture` the agent removes the flag **before** reading the card (`// The flag must go BEFORE the card is read` … `return fmt.Errorf("refusing to capture: could not clear the capture flag first, ...")` — failure to clear is fatal), and capture never opens the card for writing. So after any mid-capture death (including an OOM kernel panic from Lane A's concurrency): the flag is already gone, the sealed system is untouched on card, and a power cycle boots it normally. The sealed system self-heals: the identity service runs `ssh-keygen -A` when host keys are missing, `machine-id` regenerates, `/var/lib/rasputin/provisioned` is intact (seal keeps it — that was bug 5). The CLI already dropped its pinned host key at `internal/cluster/bake.go:102` (`c.State.ForgetHostKey(builder.Name)`, right after the capture reboot), so a re-bake reconnects cleanly.

**F3 — which agent binary runs each phase.** The capture during a bake is executed by the recovery.gz **inside `out/vanilla-custom.img.zst`** (the builder is reflashed with that image first, and boots its embedded agent for the capture). A `dryrun` runs the recovery.gz **on the node's card right now**. Therefore: Lane A's capture speedup requires a fresh `prepare` before the bake (step 2); a dryrun of the *new* agent requires an `adopt` first (step 1 — `adopt` pushes `out/recovery.gz`, which `make build` regenerates, so a dryrun does NOT need a fresh prepare). If a bake's capture shows no speedup, the first suspect is a stale `out/vanilla-custom.img.zst`.

**F4 — bake serves only the .zst.** `internal/cluster/cluster.go:107`: `var VanillaImage = Image{Name: "vanilla-custom.img.zst", Path: prepare.ImageZstPath}`. The 2.9 GB intermediate `out/vanilla-custom.img` (`internal/prepare/prepare.go:34`: `ImagePath = "out/vanilla-custom.img"`, never deleted by prepare) is safe to `rm` after prepare finishes.

**F5 — baseline numbers (hardware-measured, `plan/PROGRESS.md` T17-T21 and final summary):**

| operation | baseline | detail |
|---|---|---|
| `adopt <node>` | 40-55 s | reboot + verify, no wipe |
| `dryrun -vanilla` | 2m23s | 2,977,955,840 B decoded, 1m13s @ 40.6 MB/s; wire ~10 MB/s |
| `dryrun` (8 GiB golden) | ~5m | 8,589,934,592 B decoded, 4m2s @ 35.4 MB/s |
| `bake` | 22m30s | reflash+firstrun ~6m · provision ~4m · seal s · **capture 11m3s** (8.59 GiB read @ 12.96 MB/s, single-core zstd encode bound) · verify+return ~1m |
| `flash <node>` | 10m28s-13m18s (5 runs) | ~540 s SD write @ 15.9 MB/s; wire 4.3-4.5 MB/s (backpressure) |
| parallel flash ×2 | 12m48s / 10m29s | same per-node rate as solo; aggregate wire 8.6-9 MB/s, under the ~10 MB/s ceiling |
| `status` | 8 s | read-only |
| `prepare` (full, warm `cache/`) | ~11-30 s total | the ~11 s measured in T11 **already includes** the zstd compression (2,977,955,840 B → 735,551,529 B) |

Current golden (verified against `out/meta/golden.json`): build `20260829T021418Z-66b2f9`, **2,448,792,419 B** compressed, **8,589,934,592 B** card (`card_used_bytes` — exactly 8 GiB, the `rootfs_size_gb: 8` cap at `rasputin.yaml:6` is precise), sha256 `00c2b15a7774538e600ac449eb6a02bdfc83bfe2c0777715709c30e1598c53f5`. Pre-merge `out/meta/prepare.json`: build `20260829T021418Z-66b2f9`, `sha256_recovery` `4c6ffd2336ba901c4a6907148c4f75b8daf291ba01b7832444f8fd193805d455`. All four nodes run the golden as user `berry`; all four have passwordless sudo (`ssh.sudo: passwordless` stays — do NOT switch to `password` mode; it is not needed and drags in the `sudo -k -S` machinery for nothing).

**F6 — variance error bar.** Five baseline flashes spread 10m28s→13m18s ≈ **27% max-over-min, per SD card**. Every timing band below already includes it. **A single run must not be declared a regression (or a win) on wall time alone if it is within ±27% of expectation; re-run once, ideally on a different node, before concluding.** The *hard* pass/fail signals are binary: build id, hostname, decode verification, marker files, absence of stray capture POSTs — not seconds.

**F7 — the rootfs cap is consumed at PREPARE time, not bake time.** Verified chain: `internal/config/config.go:57`: `RootfsSizeGB int \`yaml:"rootfs_size_gb"\`` → `internal/provision/provision.go:74`: `RootfsSizeGB: cfg.Image.RootfsSizeGB,` → `internal/provision/templates/firstrun.sh.tmpl:17`: `ROOTFS_CAP_GB={{.RootfsSizeGB}}`. The cap is templated into `firstrun.sh` inside `out/vanilla-custom.img.zst` when `prepare` runs. **Editing `rasputin.yaml` between a prepare and a bake changes nothing** — the bake reflashes the builder from the already-built .zst. Every cap change therefore requires: edit yaml → `prepare` (full) → `rm out/vanilla-custom.img` → note the new build id → then bake. Also verified (`internal/prepare/prepare.go:263-273`, `currentBuildID` cached per process): **each `prepare` invocation mints a fresh build id**, so any build id or `sha256_recovery` recorded before the pre-bake prepare is superseded by it.

---

# Task D1 — write `plan/SPEEDUP.md`

Create `plan/SPEEDUP.md` containing the runbook below **verbatim in substance** (prose may be tightened; every OWNER GATE, command, number, band, and trap note must survive). During execution (Task D2) the executor appends measured values into the per-step "measured:" slots.

---

## The runbook (content of plan/SPEEDUP.md)

### Step 0 — Preflight (~15 min, nothing destructive)

- **P1. Lanes merged.** `git log --oneline -30` shows commits after `9e31059` implementing all three lanes. Cross-check with `git diff 9e31059..HEAD --stat`: Lane A touches `internal/agent/` (and possibly `internal/server/` for rate lines), Lane B touches `internal/cluster/flash.go` and/or `cmd/rasputin/flash.go`, Lane C touches `internal/provision/` and `rasputin.yaml`. Any lane absent → STOP, do not run hardware. Also confirm Lane C kept the cap config-driven through `rasputin.yaml` `rootfs_size_gb` (fact F7's chain still intact in the diff); if Lane C hard-coded the cap instead, the two-bake path of step 2 is impossible — present the one-bake path as the only option.
- **P2. Tests.** `make test` (runs `go test ./...` then `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` per the Makefile) — both green on the Mac, no hardware. `internal/prepare`'s `TestImageContents` is the end-to-end gate; it must be among the passing tests.
- **P3. Build.** `make build` — rebuilds `out/rasputin` and `out/recovery.gz` with the merged agent. (No full `prepare` here — deliberately. Fact F7: the cap is baked in at prepare time, so the one full prepare per bake happens in step 2/step 4 *after* the owner picks the cap. The step-1 dryrun does not need it: `adopt` pushes the fresh `out/recovery.gz`, and the existing pre-merge `out/vanilla-custom.img.zst` is a fine dryrun payload. If `out/vanilla-custom.img.zst` is missing, run `go run ./cmd/rasputin prepare && rm out/vanilla-custom.img` now with the yaml as merged — for a dryrun the embedded cap value is irrelevant, nothing grows.)
- **P5. BACK UP THE GOLDEN.** The only working golden image gets overwritten by a successful bake (fact F1), and mechanical verification cannot catch a behaviorally poisoned image (T20 precedent). Cheap insurance, mandatory:
  ```sh
  mkdir -p out/backup-pre-speedup
  cp -p out/golden.img.zst out/meta/golden.json out/backup-pre-speedup/
  shasum -a 256 out/backup-pre-speedup/golden.img.zst
  ```
  PASS: size 2,448,792,419 B and sha256 `00c2b15a7774538e600ac449eb6a02bdfc83bfe2c0777715709c30e1598c53f5` (must match `bytes`/`sha256` in the copied `golden.json`). `out/` is gitignored, so the backup stays uncommitted — never `git add` it.
- **P6. Disk budget.** `df -g /` — measured 2026-08-29: ~9 GB avail. Peak during bake #1 inside `out/` ≈ old golden 2.45 + backup 2.45 + staged capture ≤2.45 + vanilla .zst ~0.74 ≈ 8.1 GB (~4.9 GB above the current footprint; the transient 2.9 GB `out/vanilla-custom.img` from each prepare is deleted immediately after each prepare, but budget for it existing briefly). PASS: ≥ **6 GB** avail (8 GB comfortable). If short: move `out/backup-pre-speedup/` to another volume first; never delete `cache/` casually (it saves a 500 MB download).
- **P7. Cluster health.** `go run ./cmd/rasputin status` — 4 rows, all reachable and MAC-verified, all on build `20260829T021418Z-66b2f9`, ~8 s. **Never trust a hostname** (CLAUDE.md trap): if a node is missing, it is found by MAC/ARP, not by name — this cluster once had two nodes answering `rasputin002`. Do not weaken or bypass the MAC check to "unblock" anything.
- **P8. ssh-agent.** `ssh-add -l` shows the ED25519 key (the owner's key is passphrase-protected and used via the agent).
- **P9. Find Lane B's skip line.** `grep -n -i "skip" internal/cluster/flash.go cmd/rasputin/flash.go internal/cluster/*.go` — record (a) the exact log text, (b) the trigger: builder-name-in-`all` vs already-at-golden-build-id. This decides what step 5 must show.
- **P10. Find Lane A's concurrency knob** (encoder concurrency / window options in `internal/agent/`) — record it; it is the mitigation if bake #1 OOMs.
- **P11. Find Lane C's grow unit** (in `internal/provision/`): record the unit/script name and confirm from the code that (i) a grow failure does **not** block boot (no hard `Requires=`/blocking oneshot before `multi-user.target` without a timeout — remember the `userconfig.service` trap: a blocking oneshot on a headless node wedges systemd at "starting" forever), and (ii) it is idempotent (second boot no-op). If grow failure IS boot-blocking, the rollback section's "degraded, not bricked" claim is void — STOP and flag before flashing anything with the 4 GiB golden.
- **P12. Find Lane A's rate-log ("A2") line format.** `git diff 9e31059..HEAD -- internal/agent internal/server internal/cluster | grep -iE 'MB/s|rate'` — record the exact log-line format(s) Lane A added. This is the expectation string for the "A2 rate logging visible" checks in steps 1, 2, and 3. Baseline formats that already exist and do NOT count as A2's addition: the Mac-side progress line `%s: pulled %s` (`internal/cluster/dryrun.go:111`, `c.Log("%s: pulled %s", node.Name, ProgressLine(p))`) and the agent's final report `result: OK attempt=<n> <bytes> bytes in <dur> (<rate> MB/s)` (`internal/agent/pipeline.go:397`: `return fmt.Sprintf("OK attempt=%d %s", r.Attempt, r.Stats)`, printed under a `result: ` prefix per line 402; regression-tested at `internal/agent/pipeline_test.go:296` as `"OK attempt=2 2147483648 bytes in 1m40s (21.5 MB/s)"`).

(There is deliberately no P4; the numbering skip keeps the other P-anchors stable against the earlier draft.)

**DoD step 0:** 1) P1-P3, P5-P12 all recorded in SPEEDUP.md with values; 2) backup sha verified; 3) ≥6 GB free; 4) status shows 4/4 healthy. Any failure → stop; nothing destructive has happened.

### Step 1 — Smoke the new agent: adopt + dryrun on rasputin001 (~4 min)

**OWNER NOTICE (not a wipe): `adopt rasputin001` reboots the node once to install the new recovery.gz. Tell the owner; a full gate is not required.**

Rationale (fact F3): a dryrun executes the recovery.gz already on the node's card — the *old* agent — unless we adopt first. Adopt pushes `out/recovery.gz` (fresh from P3's `make build`) without flashing (`internal/cluster/adopt.go:83-84`: `installing recovery.gz (%d bytes)` … `conn.Push(RecoveryPath, ...)`).

```sh
go run ./cmd/rasputin adopt rasputin001      # expect 40-55 s
go run ./cmd/rasputin dryrun -vanilla rasputin001
```

`-vanilla` (defined at `cmd/rasputin/dryrun.go:15`: `useVanilla := fs.Bool("vanilla", ...)`) is deliberate: it matches the 2m23s baseline, is half the cost of the default golden dryrun (~5m — `BestImage()` at `internal/cluster/cluster.go:117` prefers the golden), and the pre-merge vanilla .zst still on disk is the same bytes the baseline measured. **Amendment to the given sequence, justified:** the bare "dryrun rasputin001 (~2m23s baseline)" instruction silently assumes the vanilla image; without `-vanilla` the golden is served and the correct baseline is ~5m/35.4 MB/s.

PASS: exit 0; report line `result: OK attempt=1 <bytes> bytes in <time> (<rate> MB/s)` (exact format per P12's baseline note; the CLI-side gate is `strings.Contains(res.Report, "result: OK")` at `internal/cluster/dryrun.go:72`) with bytes ≈ 2,977,955,840, decode ≥ 35 MB/s, total ≤ 3m30s; Lane A's A2 rate lines (exact format recorded in P12) visibly present in the output; flag cleared (the command itself fails on a stale flag). No SD write occurred (dryrun never opens the card — by design).
FAIL → fix on the Mac; nothing was harmed; do not proceed to any gate.

### Step 2 — Bake #1: OWNER-GATED, two-path decision

**Decision the runbook must present to the owner — RECOMMENDED: the two-bake path.**

- **Two bakes** (bake #1 = lanes A+B at the 8 GiB cap; bake #2 = lane C at 4 GiB): ~30-40 min extra wall time. In exchange: (i) Lane A's capture speedup is only *measurable* at 8 GiB — there is no 4 GiB baseline, so a combined bake cannot distinguish "encode got faster" from "less data"; (ii) the golden-size ±3% sanity check only means anything at 8 GiB; (iii) an OOM in bake #1 indicts Lane A's concurrency alone; (iv) bake #1's output is a *fresh, new-code, known-good 8 GiB golden* — better rollback insurance than the P5 backup.
- **One bake** (everything at once, 4 GiB): ~35 min saved, but a failure could be A, B, or C, and the headline "capture is now SD-read-bound" claim goes unmeasured.

**Pre-bake sequence — MANDATORY, in this exact order (fact F7: the cap and the agent are baked into the vanilla image at prepare time; a yaml edit alone is a no-op at bake time):**

1. Set the cap for this bake in `rasputin.yaml` (line 6 today: `rootfs_size_gb: 8     # firstrun grows rootfs to this cap...`; Lane C will have changed the value to 4). Two-bake path, bake #1: set it to **8**. One-bake path: leave Lane C's merged value (4).
2. `go run ./cmd/rasputin prepare` (full, not `-initramfs-only`) — expect ~11-30 s total with a warm `cache/` (compression included; T11 measured ~11 s for the whole thing).
3. `rm out/vanilla-custom.img` (fact F4: bake serves only the .zst; this frees 2.9 GB).
4. Record from `out/meta/prepare.json`: the fresh `build_id` (each prepare mints a new one — this build id, not any earlier one, is what bake #1 will stamp everywhere) and `sha256_recovery` (must differ from the pre-merge `4c6ffd2336ba901c4a6907148c4f75b8daf291ba01b7832444f8fd193805d455`, proving the merged agent is inside).

Skipping the prepare between the yaml edit and the bake produces a bake at the *wrong cap* whose PASS 3/PASS 4 below fail on a perfectly healthy system — do not let that happen. Note: on the two-bake path, Lane C's grow unit is still inside bake #1's image; at an 8 GiB cap it simply grows 8→32 GB on clones (a useful extra data point), but it means bake #1 is not *purely* A+B — say so in the log.

**OWNER GATE — bake #1: "This will WIPE rasputin001 (the builder) and overwrite out/golden.img.zst (backup taken in P5). Expected ~17-18 min. Proceed?" Do not run without an explicit yes.**

```sh
go run ./cmd/rasputin bake        # timestamp the start from the Mac
```

Minute-by-minute expectations (Mac-side log; two-bake path — for the one-bake path use step 4's bake bands instead):
- 0:00-~6:00 — reflash with vanilla-custom (`%s: reflashing with the prepared stock image`, `internal/cluster/bake.go:162`), firstrun, extra reboot. Baseline ~6m; lanes should not move this.
- ~6:00-~10:00 — provisioning (polled every 20 s, `bake.go:23 provisionPoll`). Baseline ~4m (apt over the LAN).
- ~10:00 — seal (seconds).
- ~10:00-~17:30 — **capture: the number this whole exercise exists to move.** `%s: captured %d MiB so far` every 10 s (`bake.go:229`). Baseline: 11m3s (encode-bound at 12.96 MB/s card-read, ~35 compressed-MiB per 10 s tick). Expected with Lane A: **5m30s-8m** — 8.59 GiB read at ~18-24 MB/s (SD-read-bound), i.e. ~50-70 compressed-MiB per tick, compressed stream ~5-6 MB/s on the wire (well under the 10 MB/s ceiling).
- ~17:30-~18:30 — verify (Mac decodes 2.4 GB) + builder returns + `verifyClone` + settle.

PASS (all required):
1. exit 0; printed total **≤ 19m**. Any total > 19m fails this PASS and triggers investigation before advancing — there is no grey zone; for calibration, > 20m30s means essentially no speedup at all (baseline 22m30s minus the F6 error bar), while 19-20m30s is a partial speedup that still needs explaining;
2. capture wall **≤ 9m30s** (if > 9m30s: fact F3/F7 diagnosis — did the pre-bake prepare actually run after the merge and after the yaml edit? does the builder's log show the new build id?);
3. reported `size:` within **±3%** of 2,448,792,419 B → **2,375,328,646-2,522,256,192 B** (concurrent block encode may cost a little ratio; outside the band → investigate before flashing). One-bake path: this band does not apply — use step 4's ~1.0-1.5 GB expectation;
4. `card_used_bytes` in `out/meta/golden.json` **exactly 8,589,934,592** (one-bake path: exactly 4,294,967,296);
5. builder back healthy: hostname `rasputin001`, the step-2 pre-bake build id, systemd running/degraded (the CLI checks all three);
6. **no OOM** — see below.

**OOM watch (new risk introduced by Lane A).** The agent is PID 1 in an initramfs on a 1 GB Pi; concurrent zstd encode multiplies window buffers. Symptom of an OOM → kernel panic: the `captured N MiB so far` line freezes at a constant value, the node vanishes from the network (no ping, no ARP refresh), and the bake eventually dies on the 45-minute `captureTimeout` (`bake.go:24`). **Recovery (verified, fact F2): power cycle the builder — the pre-capture sealed system is intact on the card (capture never writes the card, and the flag was already cleared before reading), so it boots normally and self-heals its host keys and machine-id.** Then: reduce Lane A's encoder concurrency/window (the P10 knob), rebuild (`make build && go run ./cmd/rasputin prepare && rm out/vanilla-custom.img` — the prepare is again mandatory, F7), and re-gate the bake. The old golden is untouched in this scenario (fact F1: the rename never happened).

**Trap notes for this step (CLAUDE.md):** if the CLI hangs waiting for the builder, do NOT "fix" it by clearing host keys while the node is still up — `RebootOptions.ReplacesSystem` (`internal/cluster/cluster.go:161-167`, comment: `The pinned key must then be dropped — but only once the node is actually down`) exists precisely because `waitGone`'s polling re-pins a key cleared too early. Do not touch the reboot/key ordering in cluster code as a debugging shortcut. If the builder wedges at systemd `starting` forever, check `userconfig.service` is still masked before suspecting anything else.

**DoD step 2:** pre-bake sequence 1-4 executed in order with the new build id recorded; numbered PASS list 1-6 all true; measured phase times + capture MB/s recorded in SPEEDUP.md.

### Step 3 — Flash a non-builder from bake #1's golden: OWNER-GATED (~13 min)

**OWNER GATE — "This will WIPE rasputin002. Expected ~11-14 min (7-10 min on the one-bake path). Proceed?"**

```sh
go run ./cmd/rasputin flash rasputin002
```

**Only rasputin002, in both P9 branches.** Do NOT include rasputin001 on the command line to "test the skip": that would put the just-baked builder inside the blast radius of exactly the unproven code under test, on a gate that never authorized wiping it. B2's skip line is verified conclusively at step 5's `flash all`, whose gate covers every node; note the deferral in the log.

PASS:
1. 002 comes back with the current golden's build id and hostname `rasputin002` (`verifyClone`, `internal/cluster/flash.go:128-130`, enforces both), systemd running/degraded;
2. wall time **10m30s-14m** — informational, not a gate (8 GiB golden; no lane speeds up the flash path; baseline for 002 specifically was 13m18s; the ±27% bar of fact F6 applies). One-bake path: **7-10m** (see step 4's derivation);
3. A2 rate logging (P12's exact format) visible on the flash path;
4. **poison check (T20's trap: "a capture must delete its own flag before reading the card"):** zero `capture` POSTs in the server output during/after the flash. The T20 symptom was repeated `rejected a capture POST for unknown id` lines (`internal/server/capture.go:96`) as the freshly flashed clone tried to stream its card back. Even one such line = the new golden is poisoned → STOP, rollback (below), re-diagnose the agent's flag ordering.

On the **one-bake path**, additionally run step 4's eight on-node checks here, on rasputin002 (the golden is already the 4 GiB one).

**DoD step 3:** PASS 1-4 recorded; known_hosts was cleaned automatically (flash does this — plain `ssh berry@rasputin002.local` works with no REMOTE HOST warning).

### Step 4 — Bake #2 (lane C, 4 GiB) + first 4-GiB clone: two OWNER GATES (~25 min)

Skip this step entirely on the one-bake path (its bake WAS this bake; the clone checks already ran in step 3).

**Pre-bake sequence, same mandatory order as step 2 (fact F7):** (1) restore Lane C's cap in `rasputin.yaml` → `rootfs_size_gb: 4` (whatever value Lane C merged); (2) `go run ./cmd/rasputin prepare`; (3) `rm out/vanilla-custom.img`; (4) record the new build id from `out/meta/prepare.json` — bake #2 stamps this one.

**OWNER GATE — bake #2: "This will WIPE rasputin001 again and overwrite the (new) golden. Expected ~14-16 min. Proceed?"**

Expected band derivation: reflash+firstrun+provision+seal ~10m (unchanged) + capture ~3-4m48s (4,294,967,296 B read at 15-24 MB/s) + verify+return ~1m = **14-16m total**. PASS:
1. exit 0, `card_used_bytes` **exactly 4,294,967,296**;
2. golden size **roughly half**: expect ~1.0-1.5 GB (the 8 GiB image compressed to 2.45 GB; the removed 4 GiB is mostly-empty ext4 which compresses best, so "half" is approximate — **record the actual number; size is informational**, the decode verification and the clone below are the real gates);
3. builder back healthy on the bake #2 build id.

**OWNER GATE — "This will WIPE rasputin003. Expected ~7-10 min. Proceed?"** (003 is a good pick: any residual weirdness from its duplicate-hostname history surfaces here.)

`go run ./cmd/rasputin flash rasputin003` — expected **7-10m**: transfer+write ≈ half the baseline's ~540 s ≈ 4m30s (download ~1.0-1.5 GB at 4.3-4.5 MB/s overlapping the ~4.29 GiB SD write at ~15.9 MB/s), plus the fixed ~3.5-4m of reboots/firstboot/settle.

Then **on the node** (`ssh berry@rasputin003.local` or via IP from `status`):
```sh
lsblk -b -o NAME,SIZE /dev/mmcblk0        # p2 must span ~the whole 32,026,656,768 B card
df -h /                                   # size ≥ 27G (an ungrown clone would show ~3.5G)
hostname                                  # rasputin003
cat /etc/rasputin-release                 # build id == bake #2's
test -f /var/lib/rasputin/provisioned && echo present   # MUST print present
sudo -n journalctl -u rasputin-provision.service -b --no-pager   # skipped/instant — NO apt run
sudo -n journalctl -u <P11 grow unit> -b --no-pager     # grew to full card
sudo -n reboot
# wait ~90 s, reconnect:
systemctl is-system-running               # running or degraded
sudo -n journalctl -u <P11 grow unit> -b --no-pager     # second boot: explicit no-op
```
PASS: all eight checks. The `provisioned` marker check is the live enforcement of the CLAUDE.md trap **"seal must not delete /var/lib/rasputin/provisioned"** — a clone re-running apt on first boot means seal regressed; STOP and rollback. The grow checks are Lane C's hardware acceptance.

**DoD step 4:** pre-bake sequence executed in order; bake #2 PASS 1-3 + all eight node checks recorded with actual numbers (partition bytes, df size, unit log lines).

### Step 5 — Flash the remainder in parallel: OWNER-GATED (~10 min)

**OWNER GATE — name every node the command touches: "flash all will skip nodes already on the final build if the new skip logic works, but could reflash ANY of the four if it does not — authorize rasputin001, 002, 003, 004. Expected wipes: 002 and 004 (plus 003 if the skip is builder-only rather than build-id based). Expected wall time = slowest card, ~6-10 min. Proceed?"**

`go run ./cmd/rasputin flash all` — this is the conclusive B2 verification: the P9 skip line MUST appear for rasputin001 (the builder, just baked, on the final build). If P9 found the skip is build-id-based, expect the skip line for rasputin003 too (already on bake #2's build from step 4). PASS:
1. builder (and, if build-id-based, 003) skipped with the exact P9 line — record verbatim; any skipped node must show **no reboot** (its uptime keeps counting);
2. every flashed node PASS in the results table with bake #2's build id and its own hostname;
3. wall time **6-10m**. Derivation (do not double-count the fixed overhead): per node ≈ half the baseline's ~540 s transfer+write ≈ 270 s, plus ~3.5-4m of fixed reboot/settle overhead ≈ **8-9m solo-equivalent**; parallelism adds little because T21 showed 2-way parallel costs nothing per node. **Rate caveat:** per-node rate parity with step 4's solo run is only expected for ≤2 concurrent flashes — T21's "parallel costs nothing" was measured at 2 nodes (8.6-9 MB/s aggregate, under the ~10 MB/s wire ceiling). If three nodes flash concurrently (builder-only skip), demanded aggregate ≈ 3 × 4.3 = 12.9 MB/s exceeds the ceiling, so per-node rate WILL drop ~20-25% — that is physics, not a regression. Gate instead on: aggregate wire throughput ~9-10 MB/s, and every node completing + passing `verifyClone`;
4. final `go run ./cmd/rasputin status`: 4 rows, all reachable, all on the final build id, correct hostnames.

**DoD step 5:** PASS 1-4 recorded; per-node durations tabled; concurrent-node count noted next to the rates.

### Step 6 — Bookkeeping (no gate)

1. `plan/PROGRESS.md` — append, in the house style (dated `- 2026-08-NN · SPEEDUP · ...` bullets, tables like T21's `| run | result | duration |`): the P-step outcomes, dryrun/bake/flash timings and rates, the measured capture MB/s before/after, golden sizes, the skip line verbatim, the eight node checks, and any bug found (cause → fix → regression test, matching how T19/T20 were written). Timestamps from the Mac (no-RTC trap).
2. `plan/FACTS.md` — update stale facts: new golden build id/size/sha and `card_used_bytes` 4,294,967,296; rootfs cap now 4 GiB with clones growing to full card via Lane C's unit; new bake/flash/capture timings. **Also reconcile the cluster table and prose with post-T21 reality in the same edit — it is stale beyond this lane's changes** (verified 2026-08-29: `plan/FACTS.md:9` still says `SSH works today as user \`ericovis\``, the table at lines 12-17 says `sudo -n | NO` for 002/003/004 and `its hostname is \`rasputin002\` (duplicate)` for 003, and line 54 says `Current nodes use \`ericovis\``): all four nodes run as user `berry`, all four have passwordless sudo, the duplicate hostname is fixed, and IPs/build ids are whatever the final `status` shows. (CLAUDE.md rule: FACTS is ground truth — fix it the moment it is wrong.)
3. `CLAUDE.md` — update the line `Hardware operations are destructive and slow (a flash is ~13 min, a bake ~23 min)` and the cluster build-id sentence (`All four now run the golden image (build 20260829T021418Z-66b2f9)`) with measured values. **Cross-lane flag: confirm ownership first (see open questions); serial execution means no merge conflict.**
4. Annotate `plan/SPEEDUP.md` itself with all "measured:" values.
5. Verify `rasputin.yaml` matches Lane C's merged value (`git diff rasputin.yaml` empty — on the two-bake path it was toggled to 8 and restored in step 4; on the one-bake path it was never touched). Then `git add plan/SPEEDUP.md plan/PROGRESS.md plan/FACTS.md CLAUDE.md && git commit` — message in house style; add `rasputin.yaml` **only if it actually differs from HEAD** (after a correct restore it should not). **Never push. Never add `out/` or `cache/`** (the backup and goldens stay uncommitted).

**DoD step 6:** all docs updated; `git status` clean except intended files; commit exists; no push occurred.

### Rollback (keep this section in SPEEDUP.md verbatim)

- **Restore the pre-speedup golden** (any time, e.g. poisoned image at step 3): `cp -p out/backup-pre-speedup/golden.img.zst out/golden.img.zst && cp -p out/backup-pre-speedup/golden.json out/meta/golden.json`, then OWNER GATE → `flash <nodes>` returns any node to build `20260829T021418Z-66b2f9`. Verified consistent: flash reads `out/meta/golden.json` for the expected build id and serves `out/golden.img.zst` (`cmd/rasputin/flash.go:26-48`: `meta, err := cluster.ReadGoldenMeta()` … `serving golden build %s (%d bytes)`).
- **Node stuck in the recovery agent** (unreachable over SSH, retrying): not a brick — the agent never opens the card for writing until an HTTP probe of the image URL succeeds, and retries forever. Run `go run ./cmd/rasputin serve` (registers golden + vanilla and prints the exact escape-hatch line — `cmd/rasputin/serve.go:51-53`: `ssh <node> 'echo %s | sudo tee %s && sudo reboot'`) and/or power-cycle the node.
- **Builder died mid-capture (incl. Lane A OOM):** power cycle → boots the intact sealed system (fact F2, verified against `internal/agent/run.go:159-174`). Old golden still in place (fact F1). Reduce the P10 concurrency knob, rebuild AND re-prepare (F7), re-bake.
- **Clone failed to grow (Lane C):** the node still boots — the rootfs just stays at 4 GiB (~1-1.5 GB free after the OS: degraded, not bricked; podman pulls will fill it fast). **This claim is conditional on P11's verification that the grow unit is non-blocking** — if P11 found otherwise, treat a grow failure as a wedge: power cycle, and if it wedges again, OWNER GATE → reflash from the restored 8 GiB backup golden. Fix-forward for the degraded case: run the grow script manually per Lane C's docs, or reflash.
- **Config revert:** restore `rasputin.yaml` `rootfs_size_gb` to Lane C's merged value if the two-bake toggle was left at 8 — and remember the value only takes effect at the next `prepare` (F7).

### Known-variance caveat (verbatim in SPEEDUP.md)

Per-card flash spread is ~27% (10m28s-13m18s across five baseline runs on four cards). Any single-run wall-time comparison carries that error bar: a run inside ±27% of its band is neither a regression nor a win — re-run once (different node if possible) before declaring either. Binary checks (build id, hostname, decode verification, `provisioned` marker, grow result, absence of capture POSTs) are the real gates; the capture-phase rate (MB/s over 8.59 GiB) is the one timing robust enough to compare directly, because it integrates over 5-11 minutes on the same card. Per-node parallel-flash rates additionally depend on how many nodes share the ~10 MB/s wire: parity with solo runs is only expected at ≤2 concurrent.

---

# Task D2 — execute the runbook

Follow `plan/SPEEDUP.md` top to bottom, serially. Hard rules restated: an `OWNER GATE` requires a fresh explicit yes naming every node the gate lists; after ANY failed destructive step, re-gate everything (prior authorization is void); every cap change requires prepare → rm → note build id before the bake (F7); annotate measured values into SPEEDUP.md as you go, timestamped from the Mac.

**Definition of Done (whole lane):**
1. `plan/SPEEDUP.md` exists with every gate, band, trap note, rollback path, and the variance caveat; committed.
2. Preflight P1-P3, P5-P12 executed and recorded; golden backup verified by sha256.
3. Step 1 dryrun PASS with the new agent (adopt-first sequencing respected).
4. Owner chose a bake path; every destructive step ran behind an explicit gate that named every node it could wipe.
5. Each bake was preceded, after its yaml cap was set, by a full `prepare` + `rm out/vanilla-custom.img` + a recorded fresh build id (F7); bake capture time and MB/s measured and compared to the 11m3s / 12.96 MB/s baseline (two-bake path) or noted as unmeasurable (one-bake path, owner's choice logged).
6. B2 skip line captured verbatim from the step-5 `flash all`; A2 rate lines (P12's format) observed on both dryrun and flash paths.
7. A 4 GiB clone verified on-node: grown rootfs, correct identity, no apt re-run, `provisioned` present, idempotent second boot.
8. Final `status`: 4/4 nodes on the final build.
9. `plan/PROGRESS.md`, `plan/FACTS.md` (incl. the cluster-table reconciliation) and, ownership confirmed, `CLAUDE.md` updated; one commit; **no push**; `out/`+`cache/` uncommitted; `rasputin.yaml` committed only if it differs from HEAD.
10. Rollback assets (`out/backup-pre-speedup/`) still present and verified at the end — do not delete them in cleanup.