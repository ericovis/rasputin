> **ORCHESTRATOR AMENDMENTS (post-integration review — these override the spec body below where they differ):**
>
> 1. **Savings ledger:** Task A1 claims NO flash-time saving (flash is SD-write-bound at 15.9 MB/s; decode was already proven at 35.4 MB/s). A2's 15–60 s flash saving stands. Combined-lane numbers live in `plan/speedup/README.md`; do not restate totals in commit messages beyond this lane's isolated expectations.
> 2. **`!linux` stub contingency:** as designed, no `syncrange_other.go` stub is needed — portable code references only the `RangeSyncer` interface and type-asserts; the concrete type lives in the linux-tagged file. If your implementation drifts so that ANY portable (untagged) file references a symbol defined in a linux-tagged file, create `internal/agent/syncrange_other.go` (`//go:build !linux`, mirroring `boot_other.go`) rather than weakening build tags — that file is added to the allowed set for that contingency only.
> 3. This lane may run in parallel with lanes B and C (disjoint files). It must NOT touch anything outside its allowed set, including docs — Lane D owns all documentation updates.

# Lane A spec — internal/agent: parallel capture encode (A1) + overlapped fsync in the write path (A2)

Repo: `/Users/ericovis/Code/rasputin` (branch `main`). This spec is self-contained; execute it without any other context. Read `/Users/ericovis/Code/rasputin/CLAUDE.md` first anyway — its ground rules are binding and repeated in condensed form below.

## 0. Execution model and hard rules

- **ONE agent executes this whole lane, A1 then A2, sequentially.** Both tasks edit `internal/agent/pipeline.go` and `internal/agent/pipeline_test.go`. Never split this lane across parallel executors.
- **Before your first edit, record the starting commit:** `cd /Users/ericovis/Code/rasputin && START=$(git rev-parse HEAD)` — the final lane gate diffs against it. (If your shell state does not persist between commands, write it down: `git rev-parse HEAD` now and reuse the hash literally.)
- **Files this lane may modify (exhaustive):**
  1. `/Users/ericovis/Code/rasputin/internal/agent/pipeline.go`
  2. `/Users/ericovis/Code/rasputin/internal/agent/pipeline_test.go`
  3. `/Users/ericovis/Code/rasputin/internal/agent/disk_linux.go`
  4. `/Users/ericovis/Code/rasputin/internal/agent/syncrange_linux.go` (**new file**)
- **Files this lane must NOT touch** (read them for context only): `internal/agent/run.go`, `internal/agent/mode.go`, `internal/agent/boot_linux.go`, `internal/agent/boot_other.go`, `internal/agent/netup_linux.go`, `go.mod`, `go.sum`, `Makefile`, anything under `internal/cluster`, `internal/server`, `plan/`, `README.md`, `CLAUDE.md`. **Do not run `go mod tidy`** — it would rewrite `go.mod`, which is outside this lane (this spec deliberately uses only stdlib imports so tidy is never needed).
- **No git remote exists. Never push.** Commit freely on `main`. `out/` and `cache/` are gitignored and must stay uncommitted.
- **Do NOT run `flash`, `bake`, or anything that touches the physical cluster.** Hardware operations are destructive (~13 min flash, ~23 min bake) and require owner confirmation; hardware validation is a different lane. Everything below is verified on the Mac with no hardware.
- **Do NOT create any report, notes, or numbers file anywhere** (not in `plan/`, not in the repo, not in a scratch directory). The two places for expected-hardware-numbers handoff are (a) the body of each task's commit message and (b) your final text summary to the caller. Nothing else.
- Gates that must pass after EACH task (not just at the end):
  ```sh
  cd /Users/ericovis/Code/rasputin
  go test ./...                                      # must pass on darwin, no hardware
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...   # must pass
  make test                                          # runs both of the above
  ```
- Dependency precondition: `go.mod` line 13 must read `github.com/klauspost/compress v1.19.2 // indirect` (verified 2026-08-29). If the version differs, check whether the pinned version has the option this lane depends on: `grep -n WithConcurrentBlocks ~/go/pkg/mod/github.com/klauspost/compress@<pinned-version>/zstd/encoder_options.go` (in v1.19.2 it is at `encoder_options.go:345`). If the grep finds nothing, **skip TASK A1 entirely, still execute TASK A2 in full, and report the version mismatch in your final summary — do NOT upgrade or otherwise touch the dependency.**

## Baseline measurements (hardware-verified 2026-08-28/29) and targets

| Metric | Before | Expected after |
|---|---|---|
| Capture (8,589,934,592 B card read, zstd encode, POST) | 663 s @ 12.96 MB/s, bound by single-core zstd SpeedDefault encode | ~375–450 s @ ~19–23 MB/s, bound by SD read (A1) |
| `bake` end-to-end | 22m30s | ~18m (A1) |
| golden.img.zst compressed size | 2,448,792,419 B (ratio 3.51) | up to ~1–3% larger (≤ ~2,522,000,000 B) is ACCEPTED; decoded size must remain exactly 8,589,934,592 B |
| Flash write phase (8,589,934,592 B decoded to SD) | ~540 s @ 15.9 MB/s effective, with 32 periodic blocking fsyncs + 1 final | ~480–525 s @ ~17–18 MB/s; saves 15–60 s per flash, ~20 s per bake-reflash (A2) |
| Decode capability (dryrun, unchanged) | 35.4 MB/s | unchanged — A2's fallback path keeps dryrun identical |

The Pi 3 B context that drives every decision here: 4× Cortex-A53 @1.2 GHz, **1 GB RAM**, 100 Mbit ethernet on USB2 (~10 MB/s practical), SD ~20–23 MB/s sequential read / ~16+ MB/s write. The agent binary is the initramfs `/init` — **an OOM is a kernel panic requiring a physical walk to the cluster**, so every concurrency/buffer decision below is RAM-bounded explicitly.

---

# TASK A1 — parallelize the capture zstd encoder

## A1.1 Verified anchors (re-checked 2026-08-29 against the working tree)

`internal/agent/pipeline.go:318-342` — `Upload` builds the encoder in a goroutine:

```go
// line 318
func (c *Client) Upload(ctx context.Context, src io.Reader, size int64) (Stats, error) {
	start := c.now()
	pr, pw := io.Pipe()
	counted := &countingReader{r: io.LimitReader(src, size)}

	go func() {
		enc, err := zstd.NewWriter(pw,                    // line 324
			zstd.WithEncoderLevel(zstd.SpeedDefault),     // line 325
			zstd.WithEncoderCRC(true))                    // line 326
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		_, cerr := io.CopyBuffer(enc, counted, make([]byte, CopyBufferSize))
```

Why this is slow today: `io.CopyBuffer` sees that `*zstd.Encoder` implements `io.ReaderFrom` and calls `enc.ReadFrom(counted)`. Without `WithConcurrentBlocks`, klauspost v1.19.2's streaming `ReadFrom` encodes one 128 KiB block at a time with serial history on ONE A53 core — that is the measured 12.96 MB/s. With `WithConcurrentBlocks(true)` and concurrency > 1, `ReadFrom` switches to `readFromJobs`: the caller goroutine only reads the SD into 32 MiB job buffers and computes the xxhash CRC; worker goroutines encode jobs in parallel; a flusher writes them out in order as a **valid single-frame zstd stream** (library doc on `WithConcurrentBlocks`, v1.19.2 `zstd/encoder_options.go:337-353`).

## A1.2 The edit

In `internal/agent/pipeline.go`, immediately above `func (c *Client) Upload` (i.e., above the comment block at line 315 `// Upload streams size bytes from src to the CLI as a zstd-compressed POST.`), add:

```go
// uploadConcurrency bounds the parallel zstd encode of a capture to three
// workers on the Pi 3's four A53 cores: the fourth core is left for the
// caller goroutine (SD read + xxhash CRC + memcpy into job buffers) and the
// HTTP flusher. The bound is also the RAM ceiling — see newUploadEncoder.
// NEVER pass 0 here: 0 means GOMAXPROCS, which grows the worst-case buffer
// count, and the agent is /init on a 1 GB Pi where an OOM is a kernel panic
// that needs a physical power cycle.
const uploadConcurrency = 3

// newUploadEncoder returns the encoder Upload streams a capture through.
//
// WithConcurrentBlocks switches the streaming path from one-block-at-a-time
// on a single core (measured 12.96 MB/s on an A53) to job-parallel encoding:
// 32 MiB jobs (4x the 8 MiB SpeedDefault window) with a 1 MiB overlap
// prefix, flushed in order as a normal single-frame zstd stream.
//
// RAM ceiling at concurrency 3 (klauspost/compress v1.19.2): job input
// buffers can exist in the filling slot (1), the job channel (3), the
// workers (3), the result channel (3) and the flusher (1) = 11 x 32 MiB
// = 352 MiB, plus ~64 MiB of in-flight compressed output, ~7 MiB of
// overlap prefixes and ~60 MiB of encoder window/table state: ~490 MiB
// absolute worst case, reached only if the network stalls completely (at
// which point dispatch blocks and, with all buffers pooled, allocation
// stops). Steady state is far lower (~200 MiB): the SD read at ~20-23 MB/s
// is slower than three workers' ~39 MB/s aggregate, so the queues run
// empty. Both fit a 1 GB Pi with >350 MiB headroom.
func newUploadEncoder(w io.Writer) (*zstd.Encoder, error) {
	return zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderConcurrency(uploadConcurrency),
		zstd.WithConcurrentBlocks(true))
}
```

Then replace the construction at lines 324–326:

```go
		enc, err := zstd.NewWriter(pw,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithEncoderCRC(true))
```

with:

```go
		enc, err := newUploadEncoder(pw)
```

Nothing else in `Upload` changes. Keep `zstd.SpeedDefault` — do NOT switch to `SpeedFastest` in this change (see A1.5).

Facts the comment relies on, verified in the pinned module cache (`~/go/pkg/mod/github.com/klauspost/compress@v1.19.2/zstd/`): `setDefault()` gives `windowSize: 8 << 20`; `jobSize() = max(windowSize*4, 512<<10)` = 32 MiB; `overlapSize()` = windowSize/8 = 1 MiB at SpeedDefault; `startJobWorkers` makes `jobCh` and `resultCh` each with capacity `concurrent`; `NewWriter` silently disables concurrentBlocks only when `concurrent <= 1` or a dictionary is set — we set 3 and no dict, so it stays on. Do NOT add a dictionary option.

## A1.3 Tests (add to `internal/agent/pipeline_test.go`)

New imports needed by the tests below: `crypto/sha256`, `reflect`. (`io`, `bytes`, `fmt`, `strings`, `testing`, httptest etc. are already imported.)

**(a) Seam test — the options are actually applied.** Reads two unexported primitive fields via reflect (legal without unsafe for `.Bool()`/`.Int()`; only `.Interface()` would panic). Pinned to klauspost/compress v1.19.2's layout — `Encoder{o encoderOptions{...}}` with fields `concurrentBlocks bool` and `concurrent int` — say so in a comment so a future upgrade knows why it broke:

```go
// TestUploadEncoderIsParallel pins the capture encoder's concurrency options.
// It reads unexported fields of the v1.19.2 zstd.Encoder; if the pinned
// library version changes, update the field names here.
func TestUploadEncoderIsParallel(t *testing.T) {
	enc, err := newUploadEncoder(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	o := reflect.ValueOf(enc).Elem().FieldByName("o")
	if !o.IsValid() {
		t.Fatal("zstd.Encoder no longer has field o; library layout changed")
	}
	if !o.FieldByName("concurrentBlocks").Bool() {
		t.Error("capture encoder does not have concurrent blocks enabled; capture will encode on one core at ~13 MB/s")
	}
	if got := o.FieldByName("concurrent").Int(); got != 3 {
		t.Errorf("capture encoder concurrency = %d, want exactly 3 (RAM budget on a 1 GB Pi)", got)
	}
}
```

**(b) Behavior test — a multi-job stream round-trips.** 32 MiB jobs mean any capture > 32 MiB exercises the parallel path; use 100 MiB (104,857,600 B) to force >= 3 jobs. Hash on both sides rather than buffering 100 MiB twice:

```go
// patternReader yields 4 KiB runs of a slowly-varying byte: compressible
// enough that a 100 MiB encode is fast, non-constant enough to be honest.
type patternReader struct{ off int64 }

func (p *patternReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte((p.off + int64(i)) >> 12)
	}
	p.off += int64(len(b))
	return len(b), nil
}

func TestUploadMultiJobRoundTrip(t *testing.T) {
	const size = 100 << 20 // > 3x the 32 MiB parallel job size

	want := sha256.New()
	if _, err := io.CopyN(want, &patternReader{}, size); err != nil {
		t.Fatal(err)
	}

	got := sha256.New()
	var received int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dec, err := zstd.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer dec.Close()
		n, err := io.Copy(got, dec.IOReadCloser())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		received = n
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stats, err := testClient(srv.URL+"/capture").Upload(context.Background(), &patternReader{}, size)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if stats.Bytes != size {
		t.Errorf("stats.Bytes = %d, want %d", stats.Bytes, size)
	}
	if received != size {
		t.Errorf("server decoded %d bytes, want %d", received, size)
	}
	if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Error("multi-job capture stream decoded to different bytes than the source")
	}
}
```

(This uses `zstd.NewReader(r.Body)` with default options exactly like the existing `TestUploadPostsZstdOfExactlySize` — its handler's `zstd.NewReader(r.Body)` is at pipeline_test.go:222 — frame CRC is verified by default. Note the existing decoder in `Stream` (pipeline.go:261) uses `zstd.IgnoreChecksum(false)` explicitly; do not change it.)

**(c) Existing tests keep passing unchanged** — in particular `TestUploadPostsZstdOfExactlySize` (pipeline_test.go:214) and `TestUploadFailsOnShortDisk` (pipeline_test.go:252) now implicitly exercise the jobs path with sub-job-size inputs; they must pass with zero edits. If any existing test needs editing to pass, the implementation is wrong — stop and fix the implementation.

## A1.4 Traps adjacent to A1 — what NOT to do

- **Capture-flag deletion order (CLAUDE.md trap, regression-tested):** the flag file MUST be deleted BEFORE the card is read, or the flag gets baked into the golden image and every clone re-captures itself. That logic lives in `RunMode` in `internal/agent/run.go:159-183` (the `case ModeCapture:` at line 159; the comment beginning `// The flag must go BEFORE the card is read` at lines 160-166). **This lane does not edit run.go at all.** Do not "improve" the flow, do not move `Capture` calls, do not touch `removeFlag`.
- **Mode precedence** dryrun > capture > reflash is `flagOrder` in `internal/agent/mode.go:36-44`. Read-only for this lane.
- **kmsg logging style stays:** `/dev/kmsg` is a ring buffer; do not add per-block or per-job log lines to `Upload`. The existing `capture OK: %s` / `capture attempt %d FAILED` lines in run.go already print `Stats` including MB/s (`Stats.String`, pipeline.go:90-93) — that is the per-run rate visibility the hardware lane will use. Add nothing.
- Do not change `CopyBufferSize` (pipeline.go:20, `CopyBufferSize   = 4 << 20`) or add extra buffering between the SD read and the encoder: in jobs mode the caller goroutine only does SD read + xxhash + memcpy, so after this fix the binder is the SD read at ~20–23 MB/s and double-buffering buys nothing.

## A1.5 Explicitly out of scope (recorded so nobody "helpfully" adds it)

`zstd.SpeedFastest` is a measure-later option ONLY if hardware shows capture is STILL encode-bound after this lands (i.e., measured capture rate stays ~13 MB/s and worker CPU is pegged instead of rising to ~19–23 MB/s SD-read-bound). Expected outcome is that it will NOT be needed. Do not add it now; do not add config for it.

## A1.6 Definition of Done — A1

1. `internal/agent/pipeline.go` has `uploadConcurrency = 3`, `newUploadEncoder`, and `Upload` uses it; the options are exactly `WithEncoderLevel(zstd.SpeedDefault)`, `WithEncoderCRC(true)`, `WithEncoderConcurrency(3)`, `WithConcurrentBlocks(true)` — no more, no fewer.
2. `TestUploadEncoderIsParallel` and `TestUploadMultiJobRoundTrip` added and passing.
3. All pre-existing tests pass with zero modifications: `go test ./...` green on darwin.
4. `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` succeeds.
5. `make test` succeeds.
6. `go.mod`/`go.sum` byte-identical to before (`git diff --stat` shows only the two lane files so far).
7. Commit made (message suggestion: `agent: parallel capture encode (zstd concurrent blocks, 3 workers)`), ending with the Co-Authored-By/Claude-Session trailer per harness rules. No push (no remote exists).
8. Expected hardware numbers recorded **in this commit's message body and in your final text summary — do not create any file for this, anywhere** (a different lane does the measuring; do not measure yourself): capture 663 s → ~375–450 s; compressed golden.img.zst 2,448,792,419 B → up to ~2.52 GB accepted; decoded size must stay exactly 8,589,934,592 B; bake 22m30s → ~18m.

---

# TASK A2 — overlapped writeback in the flash write path

## A2.1 Verified anchors (re-checked 2026-08-29 against the working tree)

`internal/agent/pipeline.go:281-313` — `copyWithProgress` (doc comment at 278-280), with the blocking periodic Sync at 298–304:

```go
// line 281
func (c *Client) copyWithProgress(ctx context.Context, dst Target, src io.Reader) (int64, error) {
	buf := make([]byte, CopyBufferSize)
	var written, lastReport int64
	...
			if written-lastReport >= ProgressInterval {   // line 298
				lastReport = written
				c.logf("written %d MiB", written>>20)     // line 300
				if err := dst.Sync(); err != nil {        // line 301
					return written, fmt.Errorf("sync at offset %d: %w", written, err)
				}
			}
```

`internal/agent/pipeline.go:267-274` — the FINAL full Sync in `Stream`, which **must remain untouched** (it is the durability barrier before the reboot; `sync_file_range` never flushes the device cache). It sits at line 272 **before your edits**; Edits 1 and 2 add code above it, so it will move down — anchor on it positionally: it is the `if err := dst.Sync()` immediately after the `copyWithProgress` call in `Stream`, and it must survive your edits byte-for-byte:

```go
	n, err := c.copyWithProgress(ctx, dst, dec.IOReadCloser())
	stats := Stats{Bytes: n, Duration: c.now().Sub(start)}
	if err != nil {
		return stats, err
	}
	if err := dst.Sync(); err != nil {                    // line 272 pre-edit — KEEP
		return stats, fmt.Errorf("sync after %d bytes: %w", n, err)
	}
```

`internal/agent/pipeline.go:60-63` — the Target interface:

```go
type Target interface {
	io.Writer
	Sync() error
}
```

`internal/agent/disk_linux.go:24-31` — the production Target is a bare `*os.File`:

```go
func (d *sdCard) OpenWrite() (Target, error) {
	f, err := os.OpenFile(d.path, os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	d.f = f
	return f, nil
}
```

Problem being fixed: `dst.Sync()` (fsync) every 256 MiB fully stops BOTH the SD write queue and (via decoder backpressure) the TCP stream while dirty pages drain — 32 stalls per 8 GiB flash (8,589,934,592 / 268,435,456 = 32 boundary hits) plus the final fsync. Replacement: pipelined writeback with `sync_file_range(2)` — after writing range N, start async writeback of N; at the next boundary, start N+1 and then barrier N (WAIT_BEFORE|WRITE|WAIT_AFTER), so the card stays continuously fed while dirty pages remain bounded.

**Dirty-page ceiling arithmetic (must hold, and does):** while range N+1 streams in, ranges <= N−1 have been barriered (clean); un-awaited data is at most range N (256 MiB, writeback already running) + the partial range N+1 (< 256 MiB) + one 4 MiB copy buffer ≈ **516 MiB ceiling**. A 1 GB Pi running the RAM-resident agent has ~880 MiB usable; the zstd decode path needs only ~8 MB + window. The ceiling is a guarantee independent of sysctls — in practice the kernel's `vm.dirty_ratio` throttle (~20% of RAM) plus writeback draining at the same ~16 MB/s the ranges fill at keeps actual dirty pages near one range or less. Confirmed safe; do NOT shrink `ProgressInterval` (that would also flood kmsg).

## A2.2 Design

A new optional interface in `pipeline.go` (portable, no build tags), a Linux implementation in a new file `syncrange_linux.go`, a one-line wrap in `disk_linux.go`, and a reworked loop in `copyWithProgress` that type-asserts and falls back to today's blocking Sync for any Target that doesn't implement it (darwin tests, `discardTarget` dryrun, `bufTarget` in tests — all unchanged in behavior).

**Dependency decision (made deliberately — follow it):** use the stdlib `syscall` package, which this package already uses everywhere (`internal/agent/boot_linux.go` uses `syscall.Mount`/`syscall.Sync` etc.). `syscall.SyncFileRange(fd int, off int64, n int64, flags int) error` exists for linux/arm64 (verified with `GOOS=linux GOARCH=arm64 go doc syscall.SyncFileRange` under go1.27.0). The stdlib lacks the `SYNC_FILE_RANGE_*` constants, so define them locally (values from `include/uapi/linux/fs.h`, cross-checked against x/sys `zerrors_linux.go`: WAIT_BEFORE=0x1, WRITE=0x2, WAIT_AFTER=0x4). Do NOT import `golang.org/x/sys/unix`: it works (v0.47.0 is in go.mod) but risks a reflexive `go mod tidy` rewriting go.mod, which is outside this lane. **No `syncrange_other.go` stub is needed**: unlike `boot_other.go` (whose symbols are called from portable code), nothing outside linux-tagged files references the new type — the portable side only knows the `RangeSyncer` interface and its type-assert fallback.

## A2.3 The edits

**Edit 1 — `internal/agent/pipeline.go`: add the interface** directly below the `DiscardTarget` function (after line 73 `func DiscardTarget() Target { return discardTarget{} }`):

```go
// RangeSyncer is optionally implemented by a Target that can start and wait
// on writeback of byte ranges (sync_file_range(2) on Linux). It lets
// copyWithProgress flush the just-written range in the background while the
// next one streams in, instead of stalling both the SD queue and the TCP
// stream on a full blocking Sync every interval.
//
// Range syncs bound dirty pages; they are NOT a durability barrier (they
// never flush the device cache). The final full Sync in Stream stays, and
// the reboot must never happen before it.
type RangeSyncer interface {
	// StartWriteback begins asynchronous writeback of [off, off+n).
	StartWriteback(off, n int64) error
	// AwaitWriteback blocks until writeback of [off, off+n) has completed.
	AwaitWriteback(off, n int64) error
}
```

**Edit 2 — `internal/agent/pipeline.go`: replace `copyWithProgress`** (the whole function, pre-edit lines 278–313 including its doc comment `// copyWithProgress is io.CopyBuffer with periodic logging and a periodic ...`) with:

```go
// copyWithProgress is io.CopyBuffer with periodic logging and bounded
// writeback. On a Target that implements RangeSyncer the just-written range
// is flushed asynchronously while the previous one is barriered, keeping
// the card continuously fed (write N -> start N -> await N-1); on any other
// Target it falls back to a blocking Sync per interval. Either way dirty
// pages on a 1 GB Pi stay bounded: at most the range being written plus the
// one draining, ~516 MiB worst case at the 256 MiB interval.
func (c *Client) copyWithProgress(ctx context.Context, dst Target, src io.Reader) (int64, error) {
	buf := make([]byte, CopyBufferSize)
	start := c.now()
	rs, _ := dst.(RangeSyncer)
	var written, lastReport int64
	pendingOff := int64(-1) // range with writeback started but not yet awaited
	var pendingLen int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			written += int64(nw)
			if werr != nil {
				return written, fmt.Errorf("write at offset %d: %w", written, werr)
			}
			if nw != nr {
				return written, fmt.Errorf("short write at offset %d: %d of %d bytes", written, nw, nr)
			}
			if written-lastReport >= ProgressInterval {
				rangeOff, rangeLen := lastReport, written-lastReport
				lastReport = written
				c.logf("written %d MiB (%.1f MB/s)", written>>20,
					Stats{Bytes: written, Duration: c.now().Sub(start)}.Rate()/1e6)
				switch {
				case rs != nil:
					if err := rs.StartWriteback(rangeOff, rangeLen); err != nil {
						return written, fmt.Errorf("starting writeback at offset %d: %w", rangeOff, err)
					}
					if pendingOff >= 0 {
						if err := rs.AwaitWriteback(pendingOff, pendingLen); err != nil {
							return written, fmt.Errorf("awaiting writeback at offset %d: %w", pendingOff, err)
						}
					}
					pendingOff, pendingLen = rangeOff, rangeLen
				default:
					if err := dst.Sync(); err != nil {
						return written, fmt.Errorf("sync at offset %d: %w", written, err)
					}
				}
			}
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, fmt.Errorf("read at offset %d: %w", written, rerr)
		}
	}
}
```

Notes: the still-pending final range and the tail (< 256 MiB) are deliberately NOT awaited here — `Stream`'s final `dst.Sync()` (the one immediately after the `copyWithProgress` call, pre-edit line 272) flushes and makes everything durable; leave that call exactly as it is. The progress line gains an effective-MB/s figure (this plus the existing `flash OK: <Stats>` / `capture OK: <Stats>` result lines from run.go — `Stats.String` prints `(%.1f MB/s)` — is all the instrumentation the hardware-validation lane needs; add nothing else). No test parses the old `written %d MiB` text (verified by grep; the CLI's progress display in `internal/cluster` reads its own HTTP server's byte counts, not kmsg).

**Edit 3 — new file `/Users/ericovis/Code/rasputin/internal/agent/syncrange_linux.go`:**

```go
//go:build linux

package agent

import (
	"os"
	"syscall"
)

// sync_file_range(2) flags, from include/uapi/linux/fs.h. The stdlib
// syscall package has the linux/arm64 SyncFileRange wrapper but not these
// constants.
const (
	syncFileRangeWaitBefore = 0x1
	syncFileRangeWrite      = 0x2
	syncFileRangeWaitAfter  = 0x4
)

// syncRangeTarget wraps the raw SD block device with pipelined writeback
// control, so copyWithProgress can flush range N in the background while
// range N+1 streams in. Sync is inherited from *os.File unchanged: the
// final full fsync in Stream remains the durability barrier before reboot,
// because sync_file_range never flushes the device cache.
type syncRangeTarget struct{ *os.File }

var (
	_ Target      = syncRangeTarget{}
	_ RangeSyncer = syncRangeTarget{}
)

func (t syncRangeTarget) StartWriteback(off, n int64) error {
	return syscall.SyncFileRange(int(t.Fd()), off, n, syncFileRangeWrite)
}

func (t syncRangeTarget) AwaitWriteback(off, n int64) error {
	return syscall.SyncFileRange(int(t.Fd()), off, n,
		syncFileRangeWaitBefore|syncFileRangeWrite|syncFileRangeWaitAfter)
}
```

**Edit 4 — `internal/agent/disk_linux.go`: wrap the file.** Change line 30 in `OpenWrite` from:

```go
	return f, nil
```

to:

```go
	return syncRangeTarget{f}, nil
```

(`d.f = f` on line 29 stays — `Close` still closes the underlying file.)

Error semantics: a failed `StartWriteback`/`AwaitWriteback` fails the streaming attempt exactly like a failed Sync does today; `Reflash`'s retry loop in run.go (lines 39–59) reopens the disk and **restarts from byte 0**. That is the existing, intended behavior. **Do NOT add HTTP Range resume, do NOT persist offsets, do NOT add an ENOSYS soft-fallback** (the Pi OS kernel has sync_file_range; a runtime fallback would silently hide real errors).

## A2.4 Tests (add to `internal/agent/pipeline_test.go`)

Fakes:

```go
// sizedReader returns n bytes of unspecified content as fast as possible,
// so multi-GiB write patterns can be exercised without allocating them.
type sizedReader struct{ n int64 }

func (r *sizedReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	r.n -= int64(len(p))
	return len(p), nil
}

// rangeRecorder is a Target+RangeSyncer that discards writes and records
// the writeback call pattern.
type rangeRecorder struct {
	syncs    int
	calls    []string
	awaitErr error
}

func (r *rangeRecorder) Write(p []byte) (int, error) { return len(p), nil }
func (r *rangeRecorder) Sync() error                 { r.syncs++; return nil }
func (r *rangeRecorder) StartWriteback(off, n int64) error {
	r.calls = append(r.calls, fmt.Sprintf("start %d+%d", off, n))
	return nil
}
func (r *rangeRecorder) AwaitWriteback(off, n int64) error {
	r.calls = append(r.calls, fmt.Sprintf("await %d+%d", off, n))
	return r.awaitErr
}

// plainRecorder is a Target with no RangeSyncer, for the fallback path.
type plainRecorder struct{ syncs int }

func (p *plainRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (p *plainRecorder) Sync() error                 { p.syncs++; return nil }
```

**(a) The pipelined pattern is write N → start N → await N−1.** Stream 3.5 intervals (3.5 × 268,435,456 = 939,524,096 B) straight through `copyWithProgress`; boundaries fire at exactly 256/512/768 MiB because `CopyBufferSize` (4 MiB) divides `ProgressInterval` (256 MiB):

```go
func TestCopyWithProgressPipelinesWriteback(t *testing.T) {
	c := testClient("http://unused")
	rec := &rangeRecorder{}
	const interval = int64(ProgressInterval)
	n, err := c.copyWithProgress(context.Background(), rec, &sizedReader{n: 3*interval + interval/2})
	if err != nil {
		t.Fatalf("copyWithProgress: %v", err)
	}
	if want := 3*interval + interval/2; n != want {
		t.Fatalf("wrote %d bytes, want %d", n, want)
	}
	want := []string{
		fmt.Sprintf("start %d+%d", 0*interval, interval),
		fmt.Sprintf("start %d+%d", 1*interval, interval),
		fmt.Sprintf("await %d+%d", 0*interval, interval),
		fmt.Sprintf("start %d+%d", 2*interval, interval),
		fmt.Sprintf("await %d+%d", 1*interval, interval),
	}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Errorf("writeback calls = %v, want %v", rec.calls, want)
	}
	if rec.syncs != 0 {
		t.Errorf("copyWithProgress called Sync %d times on a RangeSyncer target; the blocking stall is back", rec.syncs)
	}
}
```

**(b) The final durability Sync in Stream survives for RangeSyncer targets** (small payload, no boundary hit, exactly the one final Sync):

```go
func TestStreamStillSyncsRangeSyncerTargets(t *testing.T) {
	image := zstdOf(t, bytes.Repeat([]byte("z"), 100000))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(image)
	}))
	defer srv.Close()

	rec := &rangeRecorder{}
	if _, err := testClient(srv.URL).Stream(context.Background(), rec); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if rec.syncs != 1 {
		t.Errorf("Stream called Sync %d times, want exactly the final durability barrier", rec.syncs)
	}
}
```

**(c) Fallback for plain Targets is unchanged** (2.5 intervals → exactly 2 periodic Syncs):

```go
func TestCopyWithProgressFallbackStillSyncs(t *testing.T) {
	c := testClient("http://unused")
	rec := &plainRecorder{}
	const interval = int64(ProgressInterval)
	if _, err := c.copyWithProgress(context.Background(), rec, &sizedReader{n: 2*interval + interval/2}); err != nil {
		t.Fatalf("copyWithProgress: %v", err)
	}
	if rec.syncs != 2 {
		t.Errorf("periodic syncs = %d, want 2 (dirty pages would grow unbounded on a 1 GB Pi)", rec.syncs)
	}
}
```

**(d) Await errors name the offset and fail the attempt:**

```go
func TestCopyWithProgressAwaitErrorFailsAttempt(t *testing.T) {
	c := testClient("http://unused")
	rec := &rangeRecorder{awaitErr: fmt.Errorf("card fell out")}
	const interval = int64(ProgressInterval)
	_, err := c.copyWithProgress(context.Background(), rec, &sizedReader{n: 3 * interval})
	if err == nil || !strings.Contains(err.Error(), "awaiting writeback at offset 0") {
		t.Fatalf("err = %v, want an awaiting-writeback error naming offset 0", err)
	}
}
```

These four tests stream ~1–3 GiB through no-op readers/writers; that is memory-free and takes well under a second. Existing tests (`TestStreamDecodesAndCountsBytes` at pipeline_test.go:77 asserts syncs on a plain `bufTarget`; `TestCopyWithProgressPropagatesWriteErrors` at pipeline_test.go:276 on `failTarget`) must pass unmodified.

## A2.5 Traps adjacent to A2 — what NOT to do

- **Never open the card before the probe succeeds:** `Reflash` in run.go (lines 39–59) calls `WaitProbe` (run.go:41) before `writeToDisk` (run.go:46) — this lane does not edit run.go; do not reorder anything, do not open the disk earlier "to warm it up".
- **Retry restarts from byte 0** (run.go:56 `the card may be partially written; retrying (recovery is RAM-resident)`): preserved by keeping all pipelining state local to one `copyWithProgress` call. Do NOT add Range-resume, offset persistence, or partial-retry logic in this lane.
- **The final `dst.Sync()` in `Stream` — the one immediately after the `copyWithProgress` call (pre-edit pipeline.go:272; it moves down as Edits 1–2 add code above it) — is the durability barrier before the reboot into the fresh image.** `sync_file_range` does not flush the device write cache and is NOT a substitute. If that call is missing after your edit, a node can reboot into a torn image. It must remain, byte-for-byte where it is in `Stream`.
- **kmsg is a ring buffer:** the only logging change allowed is the MB/s suffix on the existing 256 MiB-interval progress line. No per-range log lines.
- The SSH host-key trap (`RebootOptions.ReplacesSystem`), go-diskfs FAT over-read, no-RTC, and `sudo -k -S` traps are all CLI-side and untouched by this lane — if you find yourself near them, you have left the lane.

## A2.6 Definition of Done — A2

1. `RangeSyncer` interface added to pipeline.go; `copyWithProgress` implements start-N-then-await-N−1 pipelining with the plain-Sync fallback, exactly as specified (including error message texts `starting writeback at offset %d` / `awaiting writeback at offset %d`).
2. `Stream`'s final `dst.Sync()` — the one immediately after the `copyWithProgress` call — is unchanged; verify with `git diff` that no hunk touches those three lines (do not rely on the pre-edit line number 272, which will have shifted).
3. New file `internal/agent/syncrange_linux.go` exists with the `//go:build linux` tag, stdlib-`syscall` implementation, local flag constants (0x1/0x2/0x4), and the two `var _` compile-time interface assertions. No `syncrange_other.go` (nothing portable references the type).
4. `disk_linux.go` `OpenWrite` returns `syncRangeTarget{f}`.
5. New tests (a)–(d) pass; all pre-existing tests pass unmodified; `go test ./...` green on darwin (proves the !linux fallback path, since darwin never compiles syncrange_linux.go).
6. `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` succeeds (proves the linux file compiles and `syscall.SyncFileRange` resolves on arm64).
7. `make test` succeeds. `go.mod`/`go.sum` untouched.
8. Progress log line is now `written %d MiB (%.1f MB/s)`; result lines `flash OK: ... (%.1f MB/s)` and `capture OK: ...` still come from `Stats.String` unchanged.
9. Commit made (suggestion: `agent: pipeline writeback with sync_file_range instead of blocking fsync every 256 MiB`), with the standard trailer. No push.
10. Expected hardware numbers recorded **in this commit's message body and in your final text summary — do not create any file for this, anywhere** (a different lane does the measuring; do not measure yourself): write-phase effective rate 15.9 MB/s → ~17–18 MB/s; flash total 10m28s–13m18s shrinks by 15–60 s; kmsg should show 32 progress lines with monotonically-sane rates and no multi-second gaps at each 256 MiB boundary; the honest range stays 15–60 s until the validation lane's raw-card measurement.

---

# Final lane gate

Run, in order, from `/Users/ericovis/Code/rasputin`:

```sh
go test ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
make test
git status   # must show a clean tree after commits; out/ and cache/ stay untracked
git diff --stat "$START"..HEAD   # $START = the commit recorded in section 0 before your first edit
```

If `$START` was lost, `git diff --stat HEAD~2..HEAD` is equivalent after the two prescribed commits (one for A1, one for A2; adjust the count if you made a different number of commits — the point is to diff against the pre-lane tip).

The diff stat must list ONLY: `internal/agent/pipeline.go`, `internal/agent/pipeline_test.go`, `internal/agent/disk_linux.go`, `internal/agent/syncrange_linux.go`. If anything else changed, revert it before finishing.