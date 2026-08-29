> **ORCHESTRATOR AMENDMENTS (post-integration review — these override the spec body below where they differ):**
>
> 1. **Test assertions must derive the cap from config, not a literal.** Where the spec body says to change `"ROOTFS_CAP_GB=8"` to `"ROOTFS_CAP_GB=4"` (steps 6a and 6c), instead assert `fmt.Sprintf("ROOTFS_CAP_GB=%d", cfg.Image.RootfsSizeGB)` using the config `provision_test.go` already loads from `../../rasputin.yaml`. Reason: Lane D's hardware runbook temporarily toggles `rootfs_size_gb` back to 8 mid-validation; a `=4` literal would break `go test ./...` while destructive hardware work is in flight. Everything else in steps 6a/6c stands.
> 2. **Keep the cap config-driven end to end** (it is today: `firstrun.sh.tmpl` `ROOTFS_CAP_GB={{.RootfsSizeGB}}` ← `provision.go:74`). The only place the number 4 may appear is `rasputin.yaml`.
> 3. **Do not touch docs** — `CLAUDE.md`, `README.md`, `plan/*` belong to Lane D, which updates them with measured values after hardware validation. Both open questions are resolved: docs → Lane D; `internal/config` `DefaultRootfsSizeGB` stays 8, untouched.
> 4. **Report the artifact form in your final summary** so Lane D's preflights know what to look for: grow logic inside `identity.sh.tmpl` (no new template file, no `provision.go` change), marker `/var/lib/rasputin/grow-rootfs` created by seal, deleted by the identity script on successful grow.
> 5. This lane may run in parallel with lanes A and B (disjoint files).

# Lane C spec — 4 GiB golden image with marker-gated clone grow

Repo: `/Users/ericovis/Code/rasputin` (Go CLI that reflashes a 4-node Raspberry Pi 3 cluster).
Read `/Users/ericovis/Code/rasputin/CLAUDE.md` before starting; its ground rules and "Traps that have already bitten" are binding. Non-negotiables: **no git remote, never push**; `go test ./...` must pass on darwin with no hardware; `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` must pass; `out/` and `cache/` stay uncommitted.

## 0. What this change is and why the naive version is fatal

Today `rasputin.yaml` sets `rootfs_size_gb: 8`. `firstrun.sh` (rendered from `internal/provision/templates/firstrun.sh.tmpl`, baked into the image at prepare time) grows the builder's root partition to exactly that cap on first boot. A bake then captures the card, and the capture size is **the partition-table end**, computed by `internal/mbr/mbr.go:102-111`:

```go
// UsedBytes is the parsed-table form of the package-level UsedBytes.
func (t *Table) UsedBytes() int64 {
	var max int64
	for _, p := range t.Partitions {
		if end := p.EndBytes(); end > max {
			max = end
		}
	}
	return max
}
```

The recovery agent uses it via `internal/agent/disk_linux.go:40` (`used, err := mbr.UsedBytes(f)`) to decide how many bytes of card to stream. That is why the current golden is exactly 8,589,934,592 B decoded (2,448,792,419 B compressed, build `20260829T021418Z-66b2f9`). Measured on hardware: flash spends ~540 s writing those 8.59 GiB to SD at ~15.9 MB/s (SD-write-bound); bake's capture reads them at 12.96 MB/s in 11m3s (single-core zstd encode-bound).

**Goal:** bake the golden with a 4 GiB rootfs, and have each clone grow its own filesystem to the whole card on first boot. Expected: flash write phase ~540 s → ~270 s (flash total ~13 min → ~8–9.5 min); capture ~11 min → ~5.5 min (bake total ~22.5 min → ~17 min).

**Why the naive version (unconditional grow in a per-boot script) is fatal:** provision templates are rendered at prepare time and baked into the golden, and `rasputin-identity` runs on EVERY boot (`identity.service.tmpl`: `WantedBy=sysinit.target`, `Type=oneshot`, runs `/usr/local/sbin/rasputin-identity`). An unconditional grow would run **on the builder** between firstrun and seal, inflating the partition back to the whole 32 GB card before capture — and since capture size = partition-table end, the golden would be ~32 GB.

**The fix — marker gating.** The grow in the identity script runs ONLY when a marker file exists on the root filesystem, and the marker is created by `rasputin-seal` immediately before capture. Timeline (each step verified against the real code):

1. `Bake` (`internal/cluster/bake.go:62`, `if err := c.bakeReflash(ctx, srv, *builder); err != nil {`) reflashes the builder from the vanilla image → partition table reset to the stock layout (~2.77 GiB end). No marker exists (prepare never touches the ext4 root: `internal/prepare/prepare.go` package comment — "The ext4 root partition is untouched").
2. firstrun grows the rootfs to the cap (now 4 GiB) — `firstrun.sh.tmpl:35-57`, proven to work online, same boot, on hardware (plan/PROGRESS.md T19: "grew its rootfs to 7.4 GB correctly" under the 8 GiB cap).
3. Provisioning installs packages (`provision.service.tmpl`). The identity script runs on every one of these boots but the marker does not exist, so it never grows anything.
4. `Bake` runs seal over SSH (`bake.go:74`: `if _, err := conn.Sudo(sealScriptPath); err != nil {`). Seal now (a) verifies the used bytes fit the cap and aborts the bake otherwise, (b) creates the marker `/var/lib/rasputin/grow-rootfs`, then syncs.
5. Only after seal succeeds does the CLI write the capture flag (`bake.go:89`: `if err := WriteFlag(conn, FlagCapture, captureURL); err != nil {`) and reboot. The recovery agent deletes the capture flag before reading the card (existing behavior, regression-tested) and streams `UsedBytes` = 4 GiB. **The marker is inside the captured image — that is the mechanism, not a leak.**
6. Every clone (and the builder itself after its post-capture boot) sees the marker on first boot, grows the partition + filesystem to the whole card, and deletes the marker on success.
7. Growing clones to 32 GB can never inflate a future capture: **every bake starts by reflashing the builder from vanilla** (step 1), which resets the partition table to the stock layout before firstrun re-grows it to the cap. Even a failed capture that lets the builder boot its real system (grow to 32 GB) is harmless — the next bake reflashes from vanilla first.

## 1. Files this lane touches (exhaustive)

Modified:
1. `/Users/ericovis/Code/rasputin/rasputin.yaml`
2. `/Users/ericovis/Code/rasputin/internal/provision/templates/identity.sh.tmpl`
3. `/Users/ericovis/Code/rasputin/internal/provision/templates/seal.sh.tmpl`
4. `/Users/ericovis/Code/rasputin/internal/provision/templates/firstrun.sh.tmpl` (comment-only edit)
5. `/Users/ericovis/Code/rasputin/internal/provision/provision_test.go`

Read but deliberately NOT modified (verified no change needed):
- `internal/provision/provision.go` — `Data` already carries `RootfsSizeGB` (line 43, `RootfsSizeGB   int`, populated at line 74 from `cfg.Image.RootfsSizeGB`) and `render` uses `missingkey=error`; the seal template will start using the existing `{{.RootfsSizeGB}}` field, which requires no Go change. Do not add fields or constants.
- `internal/provision/templates/identity.service.tmpl`, `provision.service.tmpl` — unchanged.
- `internal/prepare/prepare_test.go` — `TestImageContents` was checked line by line: it asserts nothing about `rootfs_size_gb`, `ROOTFS_CAP_GB`, or partition sizes (it checks recovery.gz, nodes.conf, config.txt, cmdline.txt token absence, file presence, `sh -n`, build id). No update needed. Do not touch it.
- `internal/config/config.go` / `config_test.go` — `DefaultRootfsSizeGB = 8` (config.go:17) applies only when the YAML omits the field; `rasputin.yaml` sets it explicitly, and `config_test.go` tests the default against a synthetic minimal YAML, not the repo file. `TestLoadRepoConfig` (config_test.go:89) asserts only node count/builder/packages. Out of this lane — leave alone (see open questions).
- `internal/mbr/mbr.go`, `internal/agent/*`, `internal/cluster/bake.go` — read for the analysis above; no change. `verifyImage` (bake.go:270, `if want := table.UsedBytes(); total != want`) stays consistent automatically.

A repo-wide grep for other 8-GiB assumptions was done: `grep -rn "rootfs_size\|8589934592\|4294967296"` over `*.go`/`*.tmpl`/`*.yaml` finds only the sites listed above plus historical notes in `plan/*.md` and `CLAUDE.md` (docs are out of lane — see open questions). No code hard-codes the size; `UsedBytes` is always read from the image's partition table, so **the flash path needs no change** — it writes whatever the image says.

## 2. Task 1 — `rasputin.yaml`: cap 8 → 4, rewrite the stale comment

Anchor — `/Users/ericovis/Code/rasputin/rasputin.yaml:6` (verified):

```yaml
  rootfs_size_gb: 8     # firstrun grows rootfs to this cap (fits 32GB cards, keeps captures small)
```

Replace with:

```yaml
  rootfs_size_gb: 4     # cap for the BAKED golden only: firstrun grows the builder's
                        # rootfs to this, so a capture streams exactly this many GiB.
                        # Clones grow to the whole card on first boot, gated by the
                        # marker seal leaves at /var/lib/rasputin/grow-rootfs.
```

**Does 4 GiB fit?** The vanilla Trixie Lite arm64 image is 2,977,955,840 B total (proven by the T18 dryrun decode), of which ~512 MiB is the boot partition; the rootfs arrives nearly full at roughly 1.9–2.2 GiB used. Provisioning adds `podman curl htop vim git tmux` plus dependencies (~250–400 MB) and apt lists/journal (~200–350 MB, later cleaned by seal). Expected used at seal time: **~2.4–3.0 GiB** against a 4 GiB partition. That leaves ≥1 GiB of slack, but it is an estimate — which is why Task 3 REQUIRES a seal-time `df` guard with a 512 MiB margin that aborts the bake loudly before anything is stripped or captured.

**Grow target decision (Task 2): full card, no new config field.** Rationale: it matches stock Raspberry Pi OS behavior (whose full-card expansion hook `prepare` removes — the bare `resize` cmdline token, see `prepare_test.go:122-123`), needs no YAML/schema change, and cannot inflate future captures because every bake reflashes the builder from vanilla first (section 0, step 7). The real cards are 32 GB (~32,026,656,768 B per `plan/FACTS.md:35`).

DoD:
1. `rasputin.yaml:6` reads `rootfs_size_gb: 4` with the new comment.
2. `go test ./internal/config/` passes unchanged (nothing there pins the repo value).
3. No other YAML keys changed.

## 3. Task 2 — clone grow in `identity.sh.tmpl` (marker-gated, idempotent, never blocks boot)

Anchor — `/Users/ericovis/Code/rasputin/internal/provision/templates/identity.sh.tmpl:17-23` (verified):

```sh
log() {
    echo "rasputin: $*" > /dev/kmsg 2>/dev/null || true
    echo "$(date -Is 2>/dev/null || true) rasputin: $*" >> "$BOOT/rasputin-firstrun.log" 2>/dev/null || true
    echo "rasputin: $*"
}

mac=$(cat /sys/class/net/eth0/address 2>/dev/null || echo "")
```

Insert the grow block **between the closing `}` of `log()` (line 21) and the `mac=$(...)` line (line 23)** — i.e. BEFORE the MAC check, because the MAC branch can `exit 0` early (lines 24-27) and the grow must not depend on networking. Do NOT reorder or alter the MAC/hostname logic below it (CLAUDE.md trap: "Never trust a hostname" — the MAC-based identity flow must not be weakened).

Exact block to insert (ported from firstrun's proven `grow_rootfs`, `firstrun.sh.tmpl:35-57`, with three deliberate differences: target is the whole card instead of the cap; every failure logs, keeps the marker for a retry next boot, and returns 0 so boot is never blocked — matching this script's philosophy and its unit's `SuccessExitStatus=0 1`; the three inputs are env-overridable with real defaults so the gating is testable on darwin):

```sh
# --- one-shot rootfs grow on a freshly flashed clone -------------------------
# The golden image is baked with a small rootfs (rasputin.yaml
# image.rootfs_size_gb) so captures and flashes stay short. rasputin-seal
# drops this marker immediately before the capture, so every clone — and the
# builder itself, on its first boot after the capture — grows the root
# filesystem to the whole card exactly once, then clears the marker. This can
# never inflate a future golden image: a bake always reflashes the builder
# from the vanilla image first, which resets the partition table, and the
# marker only ever appears after seal, when the capture is already armed.
# On any failure the marker is kept so the grow retries on the next boot;
# a node that boots small can be fixed over SSH, one that does not boot
# cannot, so nothing here may block the boot.
GROW_MARKER="${RASPUTIN_GROW_MARKER:-/var/lib/rasputin/grow-rootfs}"
GROW_DISK="${RASPUTIN_GROW_DISK:-/dev/mmcblk0}"
GROW_SYS="${RASPUTIN_GROW_SYS:-/sys/block/mmcblk0/mmcblk0p2}"

grow_to_card() {
    card_bytes=$(blockdev --getsize64 "$GROW_DISK" 2>/dev/null || echo 0)
    [ "$card_bytes" -gt 0 ] 2>/dev/null || \
        { log "grow: cannot read the card size; leaving the marker for the next boot"; return 0; }
    start=$(cat "$GROW_SYS/start" 2>/dev/null || echo 0)
    [ "$start" -gt 0 ] 2>/dev/null || \
        { log "grow: cannot find the rootfs start sector; leaving the marker for the next boot"; return 0; }
    want_sectors=$((card_bytes / 512 - start))
    have_sectors=$(cat "$GROW_SYS/size" 2>/dev/null || echo 0)
    if [ "$have_sectors" -ge "$want_sectors" ]; then
        log "grow: rootfs partition already fills the card"
    else
        log "grow: extending the rootfs partition to the whole card ($card_bytes bytes)"
        printf ',%s\n' "$want_sectors" | sfdisk --no-reread -N 2 "$GROW_DISK" || \
            { log "grow: sfdisk failed; leaving the marker for the next boot"; return 0; }
        partx -u "$GROW_DISK" 2>/dev/null || partprobe "$GROW_DISK" 2>/dev/null || true
    fi
    resize2fs "${GROW_DISK}p2" || \
        { log "grow: resize2fs failed; leaving the marker for the next boot"; return 0; }
    rm -f "$GROW_MARKER"
    log "grow: rootfs now fills the card; marker cleared"
}
if [ -f "$GROW_MARKER" ]; then
    grow_to_card
fi
```

**Online resize / reboot question, answered from the working code:** firstrun's own grow (`sfdisk --no-reread -N 2` → `partx -u || partprobe` → online `resize2fs` on the mounted root) runs and completes **within a single boot** — plan/PROGRESS.md T19 records the builder having "grew its rootfs to 7.4 GB correctly" during firstrun, before firstrun's own reboot, and the subsequent 8 GiB-exact capture proves the partition end landed precisely on the cap. The same kernel path (BLKPG partition resize + ext4 online grow) therefore works for the clone path with **no extra reboot**; the 4 GiB → 32 GB online resize2fs costs single-digit seconds once per clone, negligible against the ~270 s flash saving. sfdisk, partx, resize2fs, blockdev are all in the base Raspberry Pi OS Lite image and on root, which is mounted rw by `local-fs.target` before this unit runs (`identity.service.tmpl`: `After=local-fs.target`).

**Idempotency / no-op cost:** after a successful grow the marker is deleted, so every later boot pays exactly one `[ -f "$GROW_MARKER" ]` test (identity runs at sysinit on every boot — keep the no-op path to that single stat). While the marker exists, `have_sectors >= want_sectors` skips sfdisk and re-runs only `resize2fs` (a fast no-op on an already-grown fs), then clears the marker — this also self-heals the "sfdisk succeeded, resize2fs failed" half-state.

Adjacent traps (CLAUDE.md) — what NOT to do:
- **"A Pi has no RTC"**: the marker is an empty file; do not write timestamps into it or gate on dates, and do not add any `date`-derived content beyond the existing best-effort log lines.
- **"Never trust a hostname"**: do not move the grow after the MAC lookup or touch the `awk` MAC-matching, `/etc/hostname`, or `ssh-keygen -A` logic.
- **"A capture must delete its own flag file before reading the card"**: the grow marker is NOT a boot-partition flag. Do not put it on `/boot/firmware`, do not teach the recovery agent about it, and do not "fix" the fact that it gets baked into the golden — being baked in is the mechanism.
- The unit file (`identity.service.tmpl`) must not change: `DefaultDependencies=no`, `Before=network-pre.target sshd.service ...`, `SuccessExitStatus=0 1` are all load-bearing.

DoD:
1. Block inserted at the specified anchor; nothing below it (from `mac=$(...)` to the final `exit 0` at line 68) modified.
2. Rendered script passes `sh -n` (covered by `TestShellSyntax`).
3. New behavioral tests in Task 5 pass: no tool runs without the marker (blockdev included); with the marker sfdisk gets `,61495296` for a 32,026,656,768 B card with p2 starting at sector 1056768, resize2fs runs, marker removed; on resize2fs failure the marker survives and exit code is still 0.
4. `go test ./internal/provision/` passes.

## 4. Task 3 — `seal.sh.tmpl`: fit guard first, marker creation last

Two edits. Current file verified; `set -u` at line 7, `log()` at lines 9-12, first action at line 14.

**Edit 3a — fit guard.** Anchor — `/Users/ericovis/Code/rasputin/internal/provision/templates/seal.sh.tmpl:14-15`:

```sh
log "seal: removing SSH host keys"
rm -f /etc/ssh/ssh_host_*
```

Insert IMMEDIATELY BEFORE that anchor (after the `log()` definition):

```sh
ROOTFS_CAP_GB={{.RootfsSizeGB}}

# --- fit check: refuse to capture a rootfs that does not fit the cap ---------
# This runs FIRST, before anything is stripped: an aborted seal leaves the
# builder exactly as provisioned — bootable, reachable, host keys and
# machine-id intact — so the fix is to raise image.rootfs_size_gb or trim
# provision.packages and simply re-run bake. The measurement still includes
# the apt lists and journal this script would have cleaned (~300 MB); the
# margin covers that.
cap_bytes=$((ROOTFS_CAP_GB * 1024 * 1024 * 1024))
margin_bytes=$((512 * 1024 * 1024))
used_kb=$(df -Pk / | awk 'NR==2 {print $3}')
[ -n "$used_kb" ] || { echo "rasputin-seal: ABORT: cannot measure / usage (df -Pk / gave no row). Nothing was changed." >&2; exit 1; }
used_bytes=$((used_kb * 1024))
if [ "$used_bytes" -gt "$((cap_bytes - margin_bytes))" ]; then
    echo "rasputin-seal: ABORT: / uses ${used_bytes} bytes; the ${ROOTFS_CAP_GB} GiB cap allows at most $((cap_bytes - margin_bytes)) (cap minus a 512 MiB margin). Nothing was changed. Raise image.rootfs_size_gb or trim provision.packages, re-run prepare, and bake again." >&2
    log "seal: ABORT: rootfs does not fit the ${ROOTFS_CAP_GB} GiB cap (used ${used_bytes} bytes)"
    exit 1
fi
log "seal: rootfs uses ${used_bytes} of ${cap_bytes} capped bytes; fits with >=512 MiB margin"
```

Do NOT default `used_kb` to 0 on a missing `df` row — that would silently pass the guard; the explicit `[ -n "$used_kb" ]` abort exists because an empty string inside `$((used_kb * 1024))` would otherwise be a shell arithmetic parse error whose message is far more confusing than the guard's own.

The abort messages MUST go to **stderr**: `bake` runs seal via `conn.Sudo(sealScriptPath)` (`internal/cluster/bake.go:74`) and on a non-zero exit the surfaced error contains only trimmed stderr (`internal/sshx/sshx.go:282-283`: `fmt.Errorf("%s@%s: %q exited %d: %s", ..., strings.TrimSpace(res.Stderr))`). A stdout-only message would be silently discarded. The `exit 1` aborts the bake before the capture flag is ever written (`bake.go:89` runs only after seal succeeds), so an aborted seal leaves the builder as a fully provisioned, uncaptured, reachable node.

Note `{{.RootfsSizeGB}}`: the seal template currently uses no template fields; the `Data` struct already provides `RootfsSizeGB` (`provision.go:43`, populated at line 74), and `render` uses `missingkey=error`, so this needs no Go change.

**Edit 3b — marker creation.** Anchor — `seal.sh.tmpl:32-36`:

```sh
rm -f /root/.bash_history /home/*/.bash_history 2>/dev/null || true

# Zeroing free space would make the capture compress far better, but on a
# 32 GB card it also writes tens of gigabytes to the flash. Not worth it.
sync
```

Exact placement: insert the marker block **immediately after the `rm -f /root/.bash_history /home/*/.bash_history 2>/dev/null || true` line** (line 32), leaving the `# Zeroing free space` comment and `sync` directly below it, so the marker is on disk before the CLI reboots into the capture:

```sh
# Arm the one-shot clone grow: rasputin-identity grows the rootfs to the
# whole card on the next real boot only when this marker exists. It is
# created here, immediately before capture, precisely so it gets baked into
# the golden image — the builder never sees it between firstrun and seal, so
# captures stay at the cap.
log "seal: arming the clone rootfs grow"
mkdir -p /var/lib/rasputin
: > /var/lib/rasputin/grow-rootfs
```

Adjacent traps (CLAUDE.md) — what NOT to do:
- **"`seal` must not delete `/var/lib/rasputin/provisioned`"** (regression-tested at `provision_test.go:174`): the marker lives in the same directory — do not add any cleanup of `/var/lib/rasputin/*`, and keep the existing comment block at seal.sh.tmpl:21-23 ("The provisioning marker is deliberately LEFT in place...") explaining why `provisioned` stays.
- **Capture-flag ordering**: seal must not create, read, or delete anything on `/boot/firmware`. The capture flag is written by the CLI after seal and deleted by the recovery agent before it reads the card; do not touch that.
- Seal must still never reboot/shutdown (`TestSealContent` rejects such lines) — the guard uses `exit 1`, which is fine.
- Marker collision check (done): seal cleans `/etc/ssh/ssh_host_*`, machine-id, journal, `/var/lib/apt/lists/*`, `/var/log`, bash history — none intersect `/var/lib/rasputin/grow-rootfs`.

DoD:
1. Guard is the first action after the `log()` definition, textually before `rm -f /etc/ssh/ssh_host_*`.
2. Both abort paths write to stderr and exit 1; success path logs the used/cap byte counts.
3. Marker creation sits immediately after the bash-history `rm -f` line, with the Zeroing comment and final `sync` below it; `provisioned` untouched.
4. Rendered seal passes `sh -n`; updated `TestSealContent` (Task 5) passes. Never EXECUTE the rendered seal in a test — it removes real files when run.

## 5. Task 4 — `firstrun.sh.tmpl` comment refresh (no code change)

Anchor — `/Users/ericovis/Code/rasputin/internal/provision/templates/firstrun.sh.tmpl:32-34`:

```sh
# --- grow the root filesystem to the configured cap --------------------------
# Not to the whole card: a capture streams the used prefix of the disk, and a
# 32 GB rootfs would mean a 32 GB golden image.
```

Replace the second and third comment lines with:

```sh
# Not to the whole card: a capture streams the used prefix of the disk, and a
# 32 GB rootfs would mean a 32 GB golden image. Clones DO get the whole card:
# rasputin-identity grows them on first boot, gated by the marker rasputin-seal
# leaves at /var/lib/rasputin/grow-rootfs just before capture.
```

Change comments only. The `grow_rootfs` function body (lines 35-56 plus the call at 57), the userconfig masking block (lines 85-101 — CLAUDE.md trap: **`userconfig.service` must stay masked**), and everything else in firstrun stay byte-identical. `ROOTFS_CAP_GB={{.RootfsSizeGB}}` at line 17 needs no edit — it picks up 4 from the YAML.

DoD: `sh -n` still passes; `TestFirstrunContent` (with the Task 5 update) passes.

## 6. Task 5 — tests in `internal/provision/provision_test.go`

All run on darwin with no hardware, matching the repo's testing rule. Existing helper `repoData(t)` (line 13) loads the real `../../rasputin.yaml`, so the cap change flows into rendered templates automatically.

**6a. Update `TestFirstrunContent`.** Anchor — `provision_test.go:60` (verified): `		"ROOTFS_CAP_GB=8",` → change to `"ROOTFS_CAP_GB=4",`. This is the only existing assertion anywhere in the repo that pins the 8 GiB value (checked: `TestImageContents` and `config_test.go` do not).

**6b. Extend `TestIdentityContent`** (anchor: function at lines 97-131). Add to the `IdentityFile` wants list:

```go
"GROW_MARKER=\"${RASPUTIN_GROW_MARKER:-/var/lib/rasputin/grow-rootfs}\"",
"sfdisk --no-reread -N 2",
"resize2fs",
"rm -f \"$GROW_MARKER\"",
```

**6c. Extend `TestSealContent`** (anchor: function at lines 156-183). Add wants: `"df -Pk /"`, `"exit 1"`, `": > /var/lib/rasputin/grow-rootfs"`, `"ROOTFS_CAP_GB=4"`. Add an ordering assertion — the guard must precede the first destructive action:

```go
if g, r := strings.Index(got, "df -Pk /"), strings.Index(got, "rm -f /etc/ssh/ssh_host_"); g < 0 || r < 0 || g > r {
    t.Error("the fit guard must run before seal strips anything (df guard not found before the host-key removal)")
}
```

Keep the existing assertions untouched, especially the `rm -f /var/lib/rasputin/provisioned` prohibition (line 174) and the no-reboot/shutdown scan (lines 177-182).

**6d. New behavioral gating tests.** These execute the rendered identity script with `sh`, fake tools first in `PATH`, and the `RASPUTIN_GROW_*` overrides. Running identity on darwin is safe: with no marker the grow is skipped, and `/sys/class/net/eth0/address` is absent so the script exits 0 right after the grow block without touching `/etc/hostname`. That sandboxing holds **only on darwin** — on a Linux box with eth0, run as root, the un-overridable hostname section would rewrite the machine's `/etc/hostname` and `/etc/hosts` — so every behavioral test starts with a GOOS skip guard. Two properties are non-negotiable in these tests, because the gated test is the lane's only automated regression against the fatal failure mode (an ungated grow on the builder → ~32 GB golden): (a) the **blockdev stub must write to the witness file** — it is the first tool `grow_to_card` calls, so a missing gate is caught even if everything after it bails; (b) the gated test must populate real `start`/`size` sys files, so a broken gate would proceed all the way to `sfdisk` rather than silently returning on the `echo 0` fallback. Add:

```go
// renderIdentity renders the identity script to an executable file.
func renderIdentity(t *testing.T) string {
	t.Helper()
	files, err := Render(repoData(t))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), IdentityFile)
	if err := os.WriteFile(path, files[IdentityFile], 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeTools builds a bin dir of stub sfdisk/resize2fs/blockdev/partx that
// append their invocation (and stdin, for sfdisk) to a witness file.
// blockdev records itself too: it is the first tool the grow calls, so the
// no-marker test can detect an ungated grow even if later steps bail out.
// resize2fsExit lets one test simulate a failed online resize.
func fakeTools(t *testing.T, witness string, resize2fsExit int) string {
	t.Helper()
	bin := t.TempDir()
	stubs := map[string]string{
		"blockdev":  "echo \"blockdev $*\" >> \"$W\"\necho 32026656768\n",
		"sfdisk":    "in=$(cat)\necho \"sfdisk $* <<$in>>\" >> \"$W\"\n",
		"partx":     "echo \"partx $*\" >> \"$W\"\n",
		"partprobe": "echo \"partprobe $*\" >> \"$W\"\n",
		"resize2fs": fmt.Sprintf("echo \"resize2fs $*\" >> \"$W\"\nexit %d\n", resize2fsExit),
	}
	for name, body := range stubs {
		script := "#!/bin/sh\nW='" + witness + "'\n" + body
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// growSysDir writes the sysfs stand-ins for /sys/block/mmcblk0/mmcblk0p2:
// p2 starts at sector 1056768 and is currently smaller than the 32 GB card
// (32,026,656,768 B, plan/FACTS.md), so an attempted grow must reach sfdisk.
func growSysDir(t *testing.T, dir string) string {
	t.Helper()
	sys := filepath.Join(dir, "sys")
	if err := os.MkdirAll(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys, "start"), []byte("1056768\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys, "size"), []byte("7331840\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return sys
}

// runIdentity executes the script with the grow inputs pointed at the sandbox.
func runIdentity(t *testing.T, script, bin, marker, sysDir string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("identity script execution is sandboxed only on darwin (no /sys/class/net/eth0)")
	}
	cmd := exec.Command("sh", script)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RASPUTIN_GROW_MARKER="+marker,
		"RASPUTIN_GROW_DISK="+filepath.Join(filepath.Dir(marker), "disk"),
		"RASPUTIN_GROW_SYS="+sysDir,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("identity exited non-zero: %v\n%s", err, out)
	}
}

func TestIdentityGrowIsGatedByTheMarker(t *testing.T) {
	dir := t.TempDir()
	witness := filepath.Join(dir, "witness")
	bin := fakeTools(t, witness, 0)
	sys := growSysDir(t, dir)
	// No marker, but everything else primed for a grow: if the gate were
	// missing, blockdev (and then sfdisk) would write the witness.
	runIdentity(t, renderIdentity(t), bin, filepath.Join(dir, "absent-marker"), sys)
	if _, err := os.Stat(witness); !os.IsNotExist(err) {
		data, _ := os.ReadFile(witness)
		t.Errorf("without the marker the grow ran tools:\n%s", data)
	}
}

func TestIdentityGrowRunsOnceWithMarker(t *testing.T) {
	dir := t.TempDir()
	witness := filepath.Join(dir, "witness")
	bin := fakeTools(t, witness, 0)
	marker := filepath.Join(dir, "grow-rootfs")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sys := growSysDir(t, dir)
	runIdentity(t, renderIdentity(t), bin, marker, sys)

	data, err := os.ReadFile(witness)
	if err != nil {
		t.Fatalf("the grow ran no tools: %v", err)
	}
	got := string(data)
	// want_sectors = 32026656768/512 - 1056768 = 61495296
	for _, want := range []string{"blockdev --getsize64", "sfdisk --no-reread -N 2", ",61495296", "resize2fs"} {
		if !strings.Contains(got, want) {
			t.Errorf("grow tool trace is missing %q:\n%s", want, got)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("the marker survived a successful grow; the grow would re-run every boot")
	}
}

func TestIdentityGrowKeepsMarkerWhenResizeFails(t *testing.T) {
	dir := t.TempDir()
	witness := filepath.Join(dir, "witness")
	bin := fakeTools(t, witness, 1) // resize2fs fails
	marker := filepath.Join(dir, "grow-rootfs")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sys := growSysDir(t, dir)
	// Must still exit 0: the identity service may never block a boot.
	runIdentity(t, renderIdentity(t), bin, marker, sys)
	if _, err := os.Stat(marker); err != nil {
		t.Error("the marker was cleared even though resize2fs failed; the grow would never retry")
	}
}
```

Add `"fmt"` and `"runtime"` to the test file's imports (`os`, `os/exec`, `path/filepath`, `strings`, `testing` are already imported at provision_test.go:3-11). The witness path is single-quoted into the stubs (`W='...'`); `t.TempDir()` paths never contain single quotes, and this keeps the stubs correct even if a path ever contains spaces. Do NOT write an equivalent executing test for seal — the rendered seal deletes real system files and must only ever be checked statically (`sh -n` + string assertions).

**6e. Leave alone:** `TestRenderProducesEveryFile` (still exactly 5 files — no new template file is added; the grow lives inside `identity.sh.tmpl`), `TestShellSyntax` (automatically covers the edited scripts), `TestNewDataRejectsAMissingOrEmptyKey`, `TestPackageList`.

DoD:
1. `go test ./internal/provision/ -v` — all tests pass, including the three new ones.
2. Negative check of the gating test (recommended, cheap): temporarily delete the `if [ -f "$GROW_MARKER" ]` guard from the template (leaving a bare `grow_to_card` call), run `go test ./internal/provision/ -run TestIdentityGrowIsGatedByTheMarker` and confirm it FAILS with a tool trace starting with `blockdev`; restore the template. This proves the lane's key regression test can actually catch the fatal failure mode.
3. `go test ./...` passes on darwin (no test outside provision needed changing; if anything else fails, stop and re-check rather than editing files outside the lane).
4. `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` passes (templates are embedded; a template syntax error surfaces at test time via `Render`).
5. `make test` passes (runs both of the above).

## 7. Rollout, expected measurable outcomes, and hardware validation

**State after this lands, before any bake:** `out/golden.img.zst` is still the old 8 GiB build (`20260829T021418Z-66b2f9`, 2,448,792,419 B compressed / 8,589,934,592 B decoded). Flashing from it keeps working unchanged and clones do NOT grow (that image has no marker) — the flash path takes its size from the image's own partition table (`mbr.UsedBytes`), so no CLI change is needed or allowed. A stale `out/vanilla-custom.img` also still carries `ROOTFS_CAP_GB=8` inside its baked firstrun; **`rasputin prepare` must be re-run before the next bake** so the 4 GiB cap and the new templates reach the image. (`TestImageContents` skips or passes against a stale image — it asserts no cap value — but the bake gate is the re-run.)

**Hardware validation (requires the owner's explicit go-ahead — CLAUDE.md: confirm before wiping a node; a bake wipes the builder `rasputin001`):**
1. `make build && go run ./cmd/rasputin prepare` then `go test ./...` (TestImageContents now gates the fresh image).
2. `rasputin bake`. Expect: seal log line reporting used bytes ≈ 2,400,000,000–3,100,000,000 against `cap_bytes=4294967296` (abort threshold 3,758,096,384); capture ≈ **5m30s ± 1m** (4,294,967,296 B at the measured ~12.96 MB/s encode-bound rate); bake total ≈ **17 min ± 2** (vs 22m30s). `out/meta/golden.json` must show `card_used_bytes: 4294967296` exactly (the cap arithmetic is sector-precise — the old build hit 8,589,934,592 exactly). Compressed size expected ~2.0–2.4 GB (the tail it no longer captures was mostly well-compressing empty space, so the compressed win is modest; the decoded win is the point). `verifyImage` (bake.go:270) enforces decoded bytes == partition-table end automatically.
3. Builder comes back and `verifyClone` passes (already part of `Bake`); then on the builder: `df -h /` shows ~29G (grew to the card), `test -f /var/lib/rasputin/grow-rootfs` fails (marker cleared), `test -f /var/lib/rasputin/provisioned` succeeds, `systemctl is-system-running` = `running`.
4. `rasputin flash rasputin00X` (owner picks the node). Expect total ≈ **8–9.5 min** (was 10m28s–13m18s): SD write phase ≈ 270 s (4,294,967,296 B at ~15.9 MB/s) plus ~3–4 min of reboots/first-boot/rediscovery, and the one-time clone grow adding only seconds to first boot. Verify on the clone: `df -h /` ~29G, marker gone, provisioned marker present, correct hostname from MAC.
5. Failure signatures:
   - Seal exits 1 with the `rasputin-seal: ABORT` line (surfaced through the bake error) → **the guard working, not a code bug**: the 4 GiB cap is too small for the current package set. The builder is untouched and reachable. Raise `image.rootfs_size_gb` (e.g. to 5), re-run `prepare`, bake again.
   - Golden decoded size 32 GB-ish → the marker gated nothing (grow ran on the builder pre-capture).
   - `card_used_bytes` still 8,589,934,592 → prepare was not re-run.
   - Clone `df -h /` stuck at ~3.9G with the marker still present → grow failed on the clone; read `/boot/firmware/rasputin-firstrun.log` for the `grow:` lines.

## 8. Trap checklist (CLAUDE.md), one line each

- Seal must NOT delete `/var/lib/rasputin/provisioned` — the new marker is a sibling file; add no directory-wide cleanup (regression test at provision_test.go:174 stays).
- Capture-flag ordering — the agent deletes the boot-partition capture flag before reading the card; this lane must not touch `/boot/firmware` from seal/identity, and the rootfs marker being baked into the golden is intentional.
- `userconfig.service` stays masked — firstrun edit is comment-only; do not touch lines 85-101.
- No RTC — the marker is an empty file; no timestamps from the node anywhere new.
- Never trust a hostname — identity's MAC verification logic is untouched; the grow inserts strictly above it.
- Host-key clearing only after the node is down / `sudo -k -S` / go-diskfs over-read — not in this lane's code paths; do not modify `RebootOptions`, `sshx`, or `bootfs`.
- No git remote, never push; `out/` and `cache/` stay uncommitted; hardware steps only with the owner's confirmation.