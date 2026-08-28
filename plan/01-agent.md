# Phase 1 — recovery agent (T05–T07)

The agent is `cmd/agent` (package main) + portable logic in `internal/agent`.
Everything syscall-y is `//go:build linux`; pure logic (mode selection, URL
parsing, retry/backoff, report formatting) is portable and unit-tested on
darwin. `GOOS=linux GOARCH=arm64 go build ./...` must always pass.
Behavioral spec: `legacy/initramfs/init` (port its semantics faithfully) and
the "Hard-won implementation facts" + "Safety ordering" sections of FACTS.md.

## T05 — boot, mounts, flag reading, switch_root [P]A

`cmd/agent/main.go` flow:
1. If PID != 1 and env RASPUTIN_AGENT_TEST unset → refuse (safety when run
   accidentally on a dev box).
2. Mount devtmpfs /dev, proc /proc, sysfs /sys (mkdir as needed; ignore EBUSY).
   Open /dev/kmsg logger. Recover from panics into an infinite log-loop
   (never exit: exiting PID1 panics the kernel).
3. Wait ≤10s for /dev/mmcblk0p1 and p2 to appear (stat loop, 200ms).
4. Mount p1 (vfat) ro at /boot. Mode = first existing of
   `reflash-dryrun` > `capture` > `reflash`, else normal. Read trimmed first
   line as URL override. Unmount /boot.
5. normal → switch_root sequence exactly as in FACTS.md; on failure log and
   sleep-loop (do NOT reboot-loop).
6. other modes → hand off to T07 run loop (stub until T07).

`internal/agent`: `SelectMode(files map[string]string) (Mode, URL)` — portable,
tested: priority order, URL trim, empty-file → default URL, no-default+no-URL
reflash → error mode that only logs (never wipes).

VERIFY:
```
go test ./internal/agent/
GOOS=linux GOARCH=arm64 go build ./cmd/agent
```

## T06 — network up

`internal/agent/netup_linux.go`: bring up lo + eth0 via netlink (LinkSetUp),
wait for eth0 to exist (log every 10s: "waiting for eth0 — driver missing?"),
wait for carrier (operstate), then DHCP with insomniacslk/dhcp `nclient4`:
request, apply addr+route via netlink, honor lease renewal NOT required (the
recovery session is short; take the lease once, re-DHCP only if a later HTTP
attempt fails with a network error). Retry DHCP forever, 3s interval, logging
each failure. Log final "eth0 up: <ip>". Interface name fixed: eth0 (FACTS).

VERIFY:
```
GOOS=linux GOARCH=arm64 go build ./cmd/agent
go vet ./...
```
(Real validation is hardware T18; nothing more is honestly testable here.)

## T07 — reflash / dryrun / capture pipelines

All in internal/agent, linux files for I/O, portable core logic tested with
in-memory readers/writers:

- `probe(url)`: http.Head, fall back to GET+Close on 405. 10s timeout.
- **reflash**: loop forever: probe with backoff (2s..60s, log "card untouched");
  then GET (no timeout on body, 30s header timeout) → klauspost zstd reader
  (checksum verify on) → open /dev/mmcblk0 O_WRONLY → copy in 4MiB buffers;
  every 256MiB log progress + sync; on any error: log stage + bytes written,
  close, retry whole loop after 5s ("card may be partially written; recovery
  is RAM-resident"). On success: Sync, syscall.Sync, log duration+bytes,
  reboot via syscall.Reboot(LINUX_REBOOT_CMD_RESTART).
- **dryrun**: same pipeline → io.Discard, 3 attempts max, probe capped at 5
  tries; then mount p1 rw, write `reflash-dryrun.log` (url, mac, result line
  with bytes+duration+speed), remove flag, unmount, reboot. Never touches disk.
- **capture**: URL comes from flag (CLI wrote `http://mac-ip:port/capture?...`).
  mbr.UsedBytes(/dev/mmcblk0) → log size → http POST with body = zstd-encoded
  (level: SpeedDefault, CRC on) stream of exactly that many bytes (io.LimitReader,
  4MiB buffer). Require 200 response; retry forever with backoff on failure
  (card is only read). Then mount p1 rw, remove `capture` flag, reboot.
- Shared: report MAC (read /sys/class/net/eth0/address) as X-Rasputin-Mac
  header and ?mac= param on all HTTP calls; CLI uses it for progress tracking.

Portable-core tests (darwin): pipeline copy with fake reader/writer asserting
byte counts, zstd round-trip with corrupted stream → error, dryrun report
formatting, backoff sequence capped at 60s.

VERIFY:
```
go test ./internal/agent/ -v | grep -q PASS
GOOS=linux GOARCH=arm64 go build ./cmd/agent
```
