# Phase 4 — hardware validation (T16–T21)

STRICTLY SEQUENTIAL. One task `doing` at a time, in order. Re-read the
guardrails in PLAN.md before each task. Every task here starts with
`go run ./cmd/rasputin status` and ends with appending observed timings to
plan/PROGRESS.md. If a node fails to return within a stated timeout: STOP
phase 4, write plan/BLOCKERS.md entry with everything observed (server
progress, last dmesg you have, timings) — the owner must physically inspect.
Do not proceed to another node "to see if it works there".

## T16 — hardware gate

- `rasputin status` shows rasputin001 reachable with passwordless sudo.
- Check whether rasputin002/004 gained passwordless sudo (BLOCKERS item —
  owner action `echo 'ericovis ALL=(ALL) NOPASSWD:ALL' | sudo tee
  /etc/sudoers.d/ericovis` on each). Record which nodes are eligible
  (eligible = reachable + passwordless sudo). rasputin001 alone is enough to
  proceed through T20. Note rasputin003 offline if still true.
- Confirm out/vanilla-custom.img.zst exists and prepare's image-content test
  (T11 VERIFY) passes TODAY (rerun it).
- Announce in PROGRESS: "hardware phase begins; rasputin001 will be wiped
  (owner pre-authorized, including its podman workloads)".

## T17 — adopt rasputin001

`go run ./cmd/rasputin adopt rasputin001` (with reboot check). PASS criteria
printed by the tool plus manual double-check: node back on SSH, config.txt
contains the initramfs line, `config.txt.pre-rasputin` backup exists, normal
boot unaffected (uptime reset, services running). This is the first time the
Go agent runs as PID1 on real hardware — if the node does NOT come back:
likely agent boot bug; recover plan = owner pulls SD, deletes the initramfs
line from config.txt (documented in BLOCKERS entry template). 10 min timeout.

## T18 — dryrun on rasputin001

`go run ./cmd/rasputin dryrun rasputin001` (serves vanilla-custom.img.zst).
PASS: tool prints the node's reflash-dryrun.log with `result: OK`, plausible
size (≥2.5 GB decompressed) and speed; eth0+DHCP+HTTP+zstd chain thereby
proven on hardware. Record download duration. 15 min timeout.

## T19 — bake golden on rasputin001

`go run ./cmd/rasputin bake`. This WIPES rasputin001 (vanilla flash), then
provisions (~apt install podman etc, expect 10–20 min on a Pi 3), seals,
captures (~8 GB read back, expect 15–25 min at ~100 Mbit). Total budget 45
min. PASS: out/golden.img.zst + out/meta/golden.json exist, zstd CRC verify
passed, builder back with hostname rasputin001, build_id matches, ssh as
`berry` works, `ericovis` login NO LONGER exists, `podman --version` works,
timezone America/Sao_Paulo, `sudo -n true` as berry works, password SSH auth
refused (`ssh -o PreferredAuthentications=password` fails fast).
Record every phase duration.

## T20 — flash rasputin001 from golden

The full production loop on the already-baked node (proves golden images are
self-sustaining — the flashed image must itself contain a working recovery):
`go run ./cmd/rasputin flash rasputin001`. PASS: tool reports success +
duration; node healthy per T19's checks; then run `flash rasputin001` a
SECOND time to prove repeatability, and `dryrun rasputin001` to prove the
recovery mechanism survived inside the golden image. Record durations
(expect: download ~4–8 min + write ~10–15 min + boot ~2 min).

## T21 — remaining nodes

For each ELIGIBLE node from T16 besides 001, one at a time: `adopt <node>` →
`dryrun <node>` → `flash <node>`. Then, with ≥2 flashed nodes, run one
parallel `flash <nodeA> <nodeB>` to exercise concurrent orchestration, then
`rasputin status` must show every flashed node healthy with the same
build_id and correct per-MAC hostname. Ineligible nodes (no sudo / offline):
list them in BLOCKERS with the exact owner command needed, and continue.
Final: append a timing table (per node: adopt, dryrun, flash durations) to
plan/PROGRESS.md.
