# SPEEDUP — hardware validation runbook

Lane D of `plan/speedup/`. Validates lanes A (parallel capture encode +
overlapped writeback), B (`provisionPoll` 5 s + build-id flash skip) and C
(4 GiB golden with marker-gated clone grow) against the real four-node
cluster.

Merged tree: `main` @ `8ce7425` (lane merges `57b0701` A, `9300b07` B,
`8ce7425` C, on top of `3a60e57`). Baseline for every comparison is
`plan/PROGRESS.md` T17–T21.

**Every `OWNER GATE` below needs a fresh, explicit yes from the owner, in the
session, naming every node the step could wipe. An earlier authorization
never carries forward past a failure — after any failed destructive step,
re-gate everything.** This phase is strictly serial: one physical cluster,
one destructive operation at a time (step 5's parallel flash is one
operation). Timestamps come from the Mac, never from a node — a Pi has no
RTC and `date` on a node is meaningless before NTP sync.

---

## Step 0 — Preflight (nothing destructive) — **COMPLETE, ALL PASS**

**P1. Lanes merged.** PASS.
`git diff 3a60e57..HEAD --stat` = 14 files, exactly the union of the three
lanes' allowed sets:
- A → `internal/agent/{pipeline.go,pipeline_test.go,disk_linux.go,syncrange_linux.go}`
- B → `internal/cluster/{bake.go,flash.go,cluster_test.go}`, `cmd/rasputin/{flash.go,main.go}`
- C → `internal/provision/{provision_test.go,templates/{firstrun,identity,seal}.sh.tmpl}`, `rasputin.yaml`

No docs, no `plan/`, no `go.mod`/`go.sum` touched by A/B/C.

F7 chain verified intact (the cap is still config-driven end to end, so the
two-bake path is possible):
`internal/config/config.go:57 RootfsSizeGB` → `internal/provision/provision.go:74`
→ `internal/provision/templates/firstrun.sh.tmpl:17 ROOTFS_CAP_GB={{.RootfsSizeGB}}`
and `seal.sh.tmpl:14 ROOTFS_CAP_GB={{.RootfsSizeGB}}`.
`rasputin.yaml:6` = `rootfs_size_gb: 4` (Lane C's merged value).
Lane C's tests derive the cap from the config, not a literal, so the toggle in
steps 2/4 keeps `go test ./...` green.

**P2. Tests.** PASS. `make test` green after each merge and on the final tree;
`internal/prepare TestImageContents` among the passing tests.

**P3. Build.** PASS. `make build` → build `20260829T152406Z-271c66`,
`out/recovery.gz` 2,706,307 B, sha256 `884e8911b454…`.
Note: `make build` runs `prepare -initramfs-only`, so `out/meta/prepare.json`
still records the **pre-merge** full prepare — build `20260829T021418Z-66b2f9`,
`sha256_recovery 4c6ffd2336ba901c4a6907148c4f75b8daf291ba01b7832444f8fd193805d455`.
That is the baseline the step-2 prepare must differ from.
`out/vanilla-custom.img.zst` present (735,550,846 B) — fine as the step-1
dryrun payload; its embedded cap is irrelevant, nothing grows in a dryrun.

**P5. Golden backed up.** PASS.
```sh
mkdir -p out/backup-pre-speedup
cp -p out/golden.img.zst out/meta/golden.json out/backup-pre-speedup/
shasum -a 256 out/backup-pre-speedup/golden.img.zst
```
2,448,792,419 B, sha256
`00c2b15a7774538e600ac449eb6a02bdfc83bfe2c0777715709c30e1598c53f5` — matches
`bytes`/`sha256` in the copied `golden.json` (build `20260829T021418Z-66b2f9`,
`card_used_bytes` 8,589,934,592). `out/` is gitignored; never `git add` it.

**P6. Disk budget.** PASS at threshold — **note the reduced margin.**
8.3 GiB free before the backup, **6.0 GiB after** (threshold ≥6 GB; the spec's
recorded baseline was ~9 GB). Projected minimum free: ~3.2 GiB during the
step-2 full prepare (transient 2,977,955,840 B `out/vanilla-custom.img`),
~3.65 GiB during bake #1's staged capture. Both clear, but `rm
out/vanilla-custom.img` immediately after every prepare is now load-bearing,
not hygiene. If a step ever runs short: move `out/backup-pre-speedup/` to
another volume. Never delete `cache/` casually (2.8 GiB; saves a 500 MB
download).

**P7. Cluster health.** PASS — 4/4 reachable and MAC-verified, ~2.3 s:

| node | MAC | address | ssh | hostname | build | prov | uptime |
|---|---|---|---|---|---|---|---|
| rasputin001 | b8:27:eb:01:02:03 | 192.168.0.74 | berry | rasputin001 | 20260829T021418Z-66b2f9 | yes | 11 h 56 m |
| rasputin002 | b8:27:eb:04:05:06 | 192.168.0.124 | berry | rasputin002 | 20260829T021418Z-66b2f9 | yes | 5 h 59 m |
| rasputin003 | b8:27:eb:07:08:09 | rasputin003.local | berry | rasputin003 | 20260829T021418Z-66b2f9 | yes | 6 h 42 m |
| rasputin004 | b8:27:eb:0a:0b:0c | 192.168.0.190 | berry | rasputin004 | 20260829T021418Z-66b2f9 | yes | 6 h 1 m |

**Never trust a hostname**: a missing node is found by MAC/ARP, never by name
— this cluster once had two nodes answering `rasputin002`. Do not weaken or
bypass the MAC check to unblock anything.

**P8. ssh-agent.** PASS — ED25519 key loaded
(`SHA256:z2O7hz+OsvbAuBuMk2LhAtbX+LECXlEslbp1II5Jkes`). The owner's key is
passphrase-protected and used through the agent.

**P9. Lane B's skip line — the trigger is BUILD-ID BASED, not builder-name.**
`internal/cluster/flash.go:114` guards on `!opts.Force && meta != nil &&
meta.BuildID != ""`, comparing the node's `/etc/rasputin-release` build id
against the golden's. Guard sits between the `Connect` error check (MAC
already verified) and `srv.URLFor`, so a skip leaves everything untouched.

- Log line, verbatim (`flash.go:119`):
  `<node>: already running golden build <id> — skipping (use -force to reflash anyway)`
- CLI summary (`cmd/rasputin/flash.go:63-64`): verdict `SKIP`, detail
  `already on build <id>`. A skipped node is a success (`OK()` unchanged).
- Override: `-force` (single dash), `cmd/rasputin/flash.go:16`.
- Bake's own builder reflash never consults the guard: `bakeReflash` does not
  call `flashOne`; regression-tested by `TestBakeReflashIgnoresTheSkipGuard`.

**Consequence for step 5:** because the trigger is the build id, `flash all`
after step 4 must skip **both** rasputin001 (builder, just baked) **and**
rasputin003 (flashed from bake #2 in step 4), leaving 002 and 004 to flash —
2 concurrent, which is exactly the ≤2 regime where T21 measured parallelism
as free.

**P10. Lane A's concurrency knob** (the OOM mitigation):
`internal/agent/pipeline.go:361 const uploadConcurrency = 3`, consumed by
`newUploadEncoder` (line 380) as `zstd.WithEncoderConcurrency(uploadConcurrency)`
+ `zstd.WithConcurrentBlocks(true)`. Reduce this to 2 (or 1, which restores
the old serial behaviour) if bake #1 OOMs.

**P11. Lane C's grow unit — verified non-blocking and idempotent.** PASS.
Grow logic is **inside `identity.sh.tmpl`** (function `grow_to_card`, gated by
`if [ -f "$GROW_MARKER" ]`); no new template file, no `provision.go` change;
`Render` still produces 5 files. Marker `/var/lib/rasputin/grow-rootfs` on the
**root** filesystem, created by `rasputin-seal` immediately before capture,
removed by `rasputin-identity` only after a successful grow, **kept on every
failure so the grow retries next boot**.

(i) *Non-blocking* — every failure path inside `grow_to_card` logs and
`return 0` (unreadable card size, missing start sector, `sfdisk` failure,
`resize2fs` failure). `identity.service` is `Type=oneshot`,
`SuccessExitStatus=0 1`, `WantedBy=sysinit.target` (a `Wants`, not a
`Requires` — nothing `Requires=` it), and sets no `TimeoutStartSec`, so
systemd's default start timeout bounds even a hang. This is *not* the
`userconfig.service` shape (that one waited on `/dev/tty8` forever). The
rollback section's "degraded, not bricked" claim therefore holds.
*Caveat worth knowing:* the unit is ordered `Before=sshd.service
ssh.service avahi-daemon.service network-pre.target`, so SSH does not come up
until the grow finishes or times out — expect a first boot slower by the
resize, not a wedge.

(ii) *Idempotent* — on success the marker is gone, so the second boot never
enters `grow_to_card`. Even with the marker present and the partition already
full it logs `grow: rootfs partition already fills the card` and re-runs a
no-op `resize2fs`.

Seal-side guard (Lane C): seal aborts the bake if `/` uses more than
3,758,096,384 B (the 4 GiB cap minus a 512 MiB margin); abort line starts
`rasputin-seal: ABORT:` on stderr. Success line to look for in a bake:
`seal: rootfs uses <N> of 4294967296 capped bytes; fits with >=512 MiB margin`.

**P12. Lane A's A2 rate-log format** — the expectation string for steps 1, 2
and 3: `written <N> MiB (<R> MB/s)`, emitted by `copyWithProgress` every
`ProgressInterval` (256 MiB).
Pre-existing lines that do **not** count as A2's addition: the Mac-side
`<node>: pulled <progress>` (`internal/cluster/dryrun.go:111`) and the agent's
final `result: OK attempt=<n> <bytes> bytes in <dur> (<rate> MB/s)`
(`internal/agent/pipeline.go`).

**DoD step 0: met.** P1–P3, P5–P12 recorded with values; backup sha verified;
≥6 GB free; status 4/4 healthy. Nothing destructive has happened.

---

## Step 1 — Smoke the new agent: adopt + dryrun on rasputin001 (~4 min)

**OWNER NOTICE (not a wipe): `adopt rasputin001` reboots the node once to
install the new `recovery.gz`. Tell the owner; a full gate is not required.**

A dryrun executes the `recovery.gz` already on the node's card — the *old*
agent — unless we adopt first. `adopt` pushes `out/recovery.gz` (fresh from
P3's `make build`) without flashing.

```sh
go run ./cmd/rasputin adopt rasputin001      # expect 40-55 s
go run ./cmd/rasputin dryrun -vanilla rasputin001
```

`-vanilla` is deliberate: it matches the 2m23s baseline and is half the cost
of the default golden dryrun (~5 min — `BestImage()` prefers the golden).
Without it the golden is served and the correct baseline is ~5 min / 35.4 MB/s.

**PASS:** exit 0; report line `result: OK attempt=1 <bytes> bytes in <time>
(<rate> MB/s)` with bytes ≈ 2,977,955,840, decode ≥ 35 MB/s, total ≤ 3m30s;
P12's `written <N> MiB (<R> MB/s)` lines visibly present; flag cleared (the
command itself fails on a stale flag). No SD write occurs — a dryrun never
opens the card.

FAIL → fix on the Mac; nothing was harmed; do not proceed to any gate.

- measured (adopt):
- measured (dryrun total / bytes / decode rate):
- measured (A2 rate lines seen):

---

## Step 2 — Bake #1: OWNER-GATED, two-path decision

**Decision for the owner — RECOMMENDED: the two-bake path.**

- **Two bakes** (bake #1 = lanes A+B at the 8 GiB cap; bake #2 = lane C at
  4 GiB): ~30–40 min extra wall time. In exchange: (i) Lane A's capture
  speedup is only *measurable* at 8 GiB — there is no 4 GiB baseline, so a
  combined bake cannot distinguish "encode got faster" from "less data";
  (ii) the golden-size ±3% sanity check only means anything at 8 GiB;
  (iii) an OOM in bake #1 indicts Lane A's concurrency alone; (iv) bake #1's
  output is a fresh, new-code, known-good 8 GiB golden — better rollback
  insurance than the P5 backup.
- **One bake** (everything at once, 4 GiB): ~35 min saved, but a failure
  could be A, B or C, and the headline "capture is now SD-read-bound" claim
  goes unmeasured.

**Pre-bake sequence — MANDATORY, in this exact order** (F7: the cap and the
agent are baked into the vanilla image at *prepare* time; a yaml edit alone is
a no-op at bake time):

1. Set the cap in `rasputin.yaml:6`. Two-bake path, bake #1: **8**.
   One-bake path: leave Lane C's merged **4**.
2. `go run ./cmd/rasputin prepare` (full, **not** `-initramfs-only`) — ~11–30 s
   with a warm `cache/`, compression included.
3. `rm out/vanilla-custom.img` (frees 2.9 GB; bake serves only the `.zst`).
   **Load-bearing given P6's 6.0 GiB margin.**
4. Record from `out/meta/prepare.json` the fresh `build_id` (each prepare mints
   a new one — *this* is what bake #1 stamps everywhere) and `sha256_recovery`,
   which **must differ** from the pre-merge
   `4c6ffd2336ba901c4a6907148c4f75b8daf291ba01b7832444f8fd193805d455`, proving
   the merged agent is inside.

Skipping the prepare between the yaml edit and the bake produces a bake at the
*wrong cap* whose PASS 3/PASS 4 fail on a perfectly healthy system.
On the two-bake path, Lane C's grow unit is still inside bake #1's image; at an
8 GiB cap it simply grows 8 → 32 GB on clones (a useful extra data point), but
it means bake #1 is not *purely* A+B — say so in the log.

> **OWNER GATE — bake #1: "This will WIPE rasputin001 (the builder) and
> overwrite `out/golden.img.zst` (backup taken in P5). Expected ~17–18 min.
> Proceed?"** Do not run without an explicit yes.

```sh
go run ./cmd/rasputin bake        # timestamp the start from the Mac
```

Minute-by-minute (Mac-side log; two-bake path):
- 0:00–~6:00 — reflash with vanilla-custom, firstrun, extra reboot. Baseline
  ~6 min; lanes should not move this (A2 shaves ~20 s).
- ~6:00–~10:00 — provisioning. Baseline ~4 min (apt over the LAN). B1 polls
  every 5 s instead of 20 s — worth ~8 s.
- ~10:00 — seal (seconds). Expect the `seal: rootfs uses …` line.
- ~10:00–~17:30 — **capture: the number this whole exercise exists to move.**
  `<node>: captured <N> MiB so far` every 10 s. Baseline 11m3s (encode-bound,
  12.96 MB/s card read, ~35 compressed-MiB per tick). Expected with Lane A:
  **5m30s–8m** — 8.59 GiB read at ~18–24 MB/s (SD-read-bound), ~50–70
  compressed-MiB per tick, ~5–6 MB/s on the wire (under the 10 MB/s ceiling).
- ~17:30–~18:30 — verify (Mac decodes 2.4 GB) + builder returns + `verifyClone`.

**PASS (all required):**
1. exit 0; printed total **≤ 19m**. No grey zone: >20m30s means essentially no
   speedup (baseline minus the F6 error bar); 19–20m30s is a partial speedup
   that still needs explaining.
2. capture wall **≤ 9m30s**. If longer: did the pre-bake prepare actually run
   after the merge *and* after the yaml edit? does the builder's log show the
   new build id? (A stale `out/vanilla-custom.img.zst` is suspect #1.)
3. reported `size:` within **±3%** of 2,448,792,419 B →
   **2,375,328,646–2,522,256,192 B**. Concurrent block encode may cost a little
   ratio; outside the band → investigate before flashing.
   (One-bake path: band does not apply — use step 4's ~1.0–1.5 GB expectation.)
4. `card_used_bytes` in `out/meta/golden.json` **exactly 8,589,934,592**
   (one-bake path: exactly 4,294,967,296).
5. builder back healthy: hostname `rasputin001`, the step-2 build id, systemd
   running/degraded.
6. **no OOM.**

**OOM watch (the new risk Lane A introduces).** The agent is PID 1 in an
initramfs on a 1 GB Pi; concurrent zstd encode multiplies window buffers.
Symptom of OOM → kernel panic: the `captured N MiB so far` line freezes at a
constant value, the node vanishes from the network (no ping, no ARP refresh),
and the bake eventually dies on the 45-minute capture timeout.
**Recovery (verified): power-cycle the builder.** The pre-capture sealed system
is intact on the card — capture never opens the card for writing, and the flag
was already cleared before the read — so it boots normally and self-heals its
host keys and machine-id. The old golden is untouched (the rename never
happened). Then: drop P10's `uploadConcurrency` to 2 (or 1), rebuild **and
re-prepare** (`make build && go run ./cmd/rasputin prepare && rm
out/vanilla-custom.img` — the prepare is mandatory, F7), and **re-gate** the
bake.

**Trap notes (CLAUDE.md).** If the CLI hangs waiting for the builder, do NOT
"fix" it by clearing host keys while the node is still up — `waitGone` polls
with real SSH connections and one of those re-pins the key you just cleared;
`RebootOptions.ReplacesSystem` exists precisely for this. Do not touch the
reboot/key ordering in cluster code as a debugging shortcut. If the builder
wedges at systemd `starting` forever, check `userconfig.service` is still
masked before suspecting anything else.

- measured (total / phases):
- measured (capture wall / MB/s):
- measured (golden size / card_used_bytes / build id):

---

## Step 3 — Flash a non-builder from bake #1's golden (~13 min)

> **OWNER GATE — "This will WIPE rasputin002. Expected ~11–14 min (7–10 min on
> the one-bake path). Proceed?"**

```sh
go run ./cmd/rasputin flash rasputin002
```

**Only rasputin002, in both P9 branches.** Do NOT add rasputin001 to "test the
skip": that puts the just-baked builder inside the blast radius of exactly the
unproven code under test, on a gate that never authorized wiping it. B2's skip
is verified conclusively at step 5, whose gate covers every node. Note the
deferral in the log.

**PASS:**
1. 002 returns with the current golden's build id and hostname `rasputin002`
   (`verifyClone` enforces both), systemd running/degraded;
2. wall time **10m30s–14m** — informational, not a gate (8 GiB golden; no lane
   speeds up the flash path except A2's 15–60 s; 002's own baseline was
   13m18s; the ±27% bar applies). One-bake path: **7–10 min**;
3. A2's `written <N> MiB (<R> MB/s)` lines visible on the flash path;
4. **poison check** (the T20 trap — "a capture must delete its own flag before
   reading the card"): **zero `capture` POSTs** in the server output during or
   after the flash. The T20 symptom was repeated `rejected a capture POST for
   unknown id` lines as the freshly flashed clone tried to stream its card
   back. Even one such line = the new golden is poisoned → STOP, roll back,
   re-diagnose the agent's flag ordering.

On the **one-bake path**, additionally run step 4's eight on-node checks here,
on rasputin002.

**DoD:** PASS 1–4 recorded; `known_hosts` cleaned automatically (plain
`ssh berry@rasputin002.local` works with no REMOTE HOST warning).

- measured (wall / build id / poison check):

---

## Step 4 — Bake #2 (lane C, 4 GiB) + first 4-GiB clone (~25 min, two gates)

Skip entirely on the one-bake path.

**Pre-bake sequence, same mandatory order (F7):** (1) restore
`rasputin.yaml:6` → `rootfs_size_gb: 4`; (2) `go run ./cmd/rasputin prepare`;
(3) `rm out/vanilla-custom.img`; (4) record the new build id from
`out/meta/prepare.json` — bake #2 stamps this one.

> **OWNER GATE — bake #2: "This will WIPE rasputin001 again and overwrite the
> (new) golden. Expected ~14–16 min. Proceed?"**

Band derivation: reflash+firstrun+provision+seal ~10 min (unchanged) + capture
~3–4m48s (4,294,967,296 B read at 15–24 MB/s) + verify/return ~1 min.

**PASS:**
1. exit 0, `card_used_bytes` **exactly 4,294,967,296**;
2. golden size roughly half: expect **~1.0–1.5 GB**. The removed 4 GiB is
   mostly-empty ext4 which compresses best, so "half" is approximate —
   **record the actual number; size is informational.** The decode
   verification and the clone below are the real gates;
3. builder back healthy on the bake #2 build id.

> **OWNER GATE — "This will WIPE rasputin003. Expected ~7–10 min. Proceed?"**
> (003 is a good pick: any residual weirdness from its duplicate-hostname
> history surfaces here.)

```sh
go run ./cmd/rasputin flash rasputin003
```
Expected **7–10 min**: transfer+write ≈ half the baseline's ~540 s ≈ 4m30s
(download ~1.0–1.5 GB at 4.3–4.5 MB/s overlapping the ~4.29 GiB SD write at
~16–18 MB/s), plus the fixed ~3.5–4 min of reboots/firstboot/settle.

Then **on the node** (`ssh berry@rasputin003.local`, or the IP from `status`):
```sh
lsblk -b -o NAME,SIZE /dev/mmcblk0        # p2 must span ~the whole 32,026,656,768 B card
df -h /                                   # size >= 27G (an ungrown clone shows ~3.5G)
hostname                                  # rasputin003
cat /etc/rasputin-release                 # build id == bake #2's
test -f /var/lib/rasputin/provisioned && echo present   # MUST print present
sudo -n journalctl -u rasputin-provision.service -b --no-pager   # skipped/instant — NO apt run
sudo -n journalctl -u rasputin-identity.service -b --no-pager    # grew to full card
sudo -n reboot
# wait ~90 s, reconnect:
systemctl is-system-running               # running or degraded
sudo -n journalctl -u rasputin-identity.service -b --no-pager    # second boot: no grow at all
```
**PASS: all eight checks.** The `provisioned` marker check is the live
enforcement of the CLAUDE.md trap **"seal must not delete
/var/lib/rasputin/provisioned"** — a clone re-running apt on first boot means
seal regressed; STOP and roll back. The grow checks are Lane C's hardware
acceptance: first boot logs `grow: extending the rootfs partition to the whole
card (32026656768 bytes)` then `grow: rootfs now fills the card; marker
cleared`; the second boot logs neither (the marker is gone).

- measured (bake #2 total / capture / golden size / card_used_bytes):
- measured (flash 003 wall):
- measured (eight node checks — partition bytes, df size, unit lines):

---

## Step 5 — Flash the remainder in parallel (~10 min)

> **OWNER GATE — "`flash all` will skip nodes already on the final build if the
> skip logic works, but could reflash ANY of the four if it does not —
> authorize rasputin001, rasputin002, rasputin003, rasputin004. Expected wipes:
> 002 and 004 (P9 confirmed the skip is build-id based, so 001 and 003 should
> both skip). Expected wall time = slowest card, ~6–10 min. Proceed?"**

```sh
go run ./cmd/rasputin flash all
```

This is the conclusive B2 verification.

**PASS:**
1. rasputin001 **and** rasputin003 skipped with P9's exact line — record it
   verbatim; each skipped node must show **no reboot** (uptime keeps counting);
2. every flashed node PASS in the results table with bake #2's build id and its
   own hostname;
3. wall time **6–10 min**. Per node ≈ half the baseline's ~540 s transfer+write
   ≈ 270 s plus ~3.5–4 min fixed overhead ≈ 8–9 min solo-equivalent;
   parallelism adds little at this width. **Rate caveat:** per-node parity with
   a solo run is only expected at ≤2 concurrent — which is what P9's build-id
   skip gives us here (002 and 004). Had three flashed concurrently, demanded
   aggregate ≈ 3 × 4.3 = 12.9 MB/s would exceed the ~10 MB/s wire ceiling and
   per-node rate would drop ~20–25% — physics, not a regression. Gate on
   aggregate wire ~9–10 MB/s and every node completing `verifyClone`;
4. final `go run ./cmd/rasputin status`: 4 rows, all reachable, all on the
   final build id, correct hostnames.

- measured (skip line verbatim / per-node durations / concurrency / final status):

---

## Step 6 — Bookkeeping (no gate)

1. `plan/PROGRESS.md` — append in house style (dated `- 2026-08-NN · SPEEDUP ·`
   bullets, tables like T21's `| run | result | duration |`): P-step outcomes,
   dryrun/bake/flash timings and rates, capture MB/s before and after, golden
   sizes, the skip line verbatim, the eight node checks, and any bug found
   (cause → fix → regression test, the way T19/T20 are written). Mac
   timestamps only.
2. `plan/FACTS.md` — new golden build id/size/sha and `card_used_bytes`
   4,294,967,296; rootfs cap now 4 GiB with clones growing to the full card;
   new bake/flash/capture timings. **Also reconcile the stale cluster table and
   prose in the same edit**: line 9 still says SSH works as `ericovis`, the
   table at lines 12–17 says `sudo -n | NO` for 002/003/004 and calls 003's
   hostname a duplicate, and line 54 repeats the `ericovis` claim. Reality: all
   four run as `berry`, all four have passwordless sudo, the duplicate hostname
   is fixed, IPs/build ids are whatever the final `status` shows.
3. `CLAUDE.md` — update `a flash is ~13 min, a bake ~23 min` and the cluster
   build-id sentence with **measured** values.
4. `README.md` — same, where it quotes durations or the golden's size.
5. Annotate this file's `measured:` slots.
6. Verify `git diff rasputin.yaml` is empty (the two-bake toggle was restored in
   step 4), then
   `git add plan/SPEEDUP.md plan/PROGRESS.md plan/FACTS.md CLAUDE.md README.md &&
   git commit`. Add `rasputin.yaml` **only if it actually differs from HEAD**.
   **Never push. Never `git add out/` or `cache/`.**

---

## Rollback

- **Restore the pre-speedup golden** (any time, e.g. a poisoned image at
  step 3):
  ```sh
  cp -p out/backup-pre-speedup/golden.img.zst out/golden.img.zst
  cp -p out/backup-pre-speedup/golden.json out/meta/golden.json
  ```
  then OWNER GATE → `flash <nodes>` returns any node to build
  `20260829T021418Z-66b2f9`. Consistent by construction: flash reads
  `out/meta/golden.json` for the expected build id and serves
  `out/golden.img.zst`.
- **Node stuck in the recovery agent** (unreachable over SSH, retrying): not a
  brick — the agent never opens the card for writing until an HTTP probe of the
  image URL succeeds, and it retries forever. Run
  `go run ./cmd/rasputin serve` (it registers golden + vanilla and prints the
  exact escape-hatch line) and/or power-cycle the node.
- **Builder died mid-capture (incl. a Lane A OOM):** power-cycle → boots the
  intact sealed system. Old golden still in place. Reduce P10's
  `uploadConcurrency`, rebuild **and re-prepare** (F7), re-gate, re-bake.
- **Clone failed to grow (Lane C):** the node still boots — the rootfs just
  stays at 4 GiB (~1–1.5 GB free after the OS: **degraded, not bricked**;
  podman pulls will fill it fast). P11 verified the grow is non-blocking, so
  this claim holds. Fix forward: re-create
  `/var/lib/rasputin/grow-rootfs` and reboot (the grow is idempotent and
  retries), run the grow steps by hand, or reflash.
- **Config revert:** restore `rasputin.yaml` `rootfs_size_gb` to Lane C's
  merged value (4) if the two-bake toggle was left at 8 — and remember the
  value only takes effect at the next `prepare` (F7).

## Known-variance caveat

Per-card flash spread is ~**27%** (10m28s–13m18s across five baseline runs on
four cards). Any single-run wall-time comparison carries that error bar: a run
inside ±27% of its band is neither a regression nor a win — re-run once
(different node if possible) before declaring either. The real gates are the
binary checks: build id, hostname, decode verification, the `provisioned`
marker, the grow result, and the absence of capture POSTs. The
capture-phase rate (MB/s over the whole card read) is the one timing robust
enough to compare directly, because it integrates over 3–11 minutes on the
same card. Per-node parallel-flash rates additionally depend on how many nodes
share the ~10 MB/s wire: parity with solo runs is only expected at ≤2
concurrent.
