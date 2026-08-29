> **ORCHESTRATOR AMENDMENTS (post-integration review — these override the spec body below where they differ):**
>
> 1. **`cmd/rasputin/main.go` is confirmed in-lane** (no other lane claims any `cmd/rasputin` file): apply the usage-string edit (e).5 as specified. The open question is resolved.
> 2. **Reuse, don't move:** call `buildIDFrom` (`internal/cluster/status.go:101`, package-internal) from `flash.go`. Do NOT edit `status.go` or `cluster.go`; if a helper genuinely must move, stop and flag it in your final summary instead.
> 3. **No hard-coded golden sizes:** never introduce `8589934592`, `2448792419`, or any constant derived from them — Lane C changes the golden's size. Sizes come from the image's partition table / `out/meta/golden.json` only.
> 4. **Skip-logic boundaries:** the B2 skip must be overridable with `--force`, must emit an explicit log line on skip, and bake's internal builder-reflash path must never consult it — add a test proving bake's reflash is unaffected if the spec body doesn't already include one.
> 5. This lane may run in parallel with lanes A and C (disjoint files). Do not touch docs — Lane D owns all documentation updates.

# Lane B spec — internal/cluster + cmd: provisionPoll + flash idempotency guard

Repo: `/Users/ericovis/Code/rasputin` — a Go CLI that reflashes a 4-node Raspberry Pi 3 cluster over the network. Read `/Users/ericovis/Code/rasputin/CLAUDE.md` before touching anything; every trap named below is from it and has a regression test.

## Repo rules that bind this lane (from CLAUDE.md)

- **No git remote. Never push.** Commit freely. `out/` and `cache/` are gitignored and must stay uncommitted.
- `go test ./...` must pass **on darwin with no hardware**. `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` must always pass.
- **Do not run `rasputin flash` or `rasputin bake` against the real cluster** as part of this work. Hardware operations are destructive and slow (a flash is ~10–13 min, a bake ~23 min) and require the owner's confirmation. The Definition of Done here is tests + cross-build only.

## Files this lane may touch (exhaustive — stay inside this set)

1. `/Users/ericovis/Code/rasputin/internal/cluster/bake.go` — **one constant only** (Task B1)
2. `/Users/ericovis/Code/rasputin/internal/cluster/flash.go` — guard + option/result plumbing (Task B2)
3. `/Users/ericovis/Code/rasputin/internal/cluster/cluster_test.go` — new tests (Task B2)
4. `/Users/ericovis/Code/rasputin/cmd/rasputin/flash.go` — `-force` flag wiring + summary (Task B2)
5. `/Users/ericovis/Code/rasputin/cmd/rasputin/main.go` — **one string only**: the flash entry in the command table (Task B2, edit (e).5)

Files you will READ but must NOT modify (owned elsewhere or shared): `internal/cluster/status.go` (provides `ReleaseFile` and `buildIDFrom` — already in the same package, just use them), `internal/cluster/cluster.go`, `internal/cluster/dryrun.go`, `internal/cluster/reboot_test.go`, `internal/cluster/knownhosts.go`, `internal/cluster/knownhosts_test.go`, `internal/nodes/nodes.go`, `internal/provision/templates/*` (Lane C owns templates), `internal/sshx/*`, `internal/server/*`, `internal/state/*`, `cmd/rasputin/adopt.go`.

---

# TASK B1 — provisionPoll 20s → 5s

## What and why

During a bake, `waitProvisioned` polls the builder over SSH every `provisionPoll` until `/var/lib/rasputin/provisioned` appears (apt provisioning takes ~4 min of the 22m30s bake). The poll interval is currently 20 s, so on average ~10 s is wasted after the marker appears before the bake notices. Dropping to 5 s cuts the mean overshoot to ~2.5 s: **~7.5 s saved per bake, mean**. Cost: ~48 fresh SSH connects over a 4-minute window instead of ~12 — negligible for sshd on a Pi 3.

## The edit (verified anchor)

File: `/Users/ericovis/Code/rasputin/internal/cluster/bake.go`, lines 21–26 currently read:

```go
// Bake stages, in order.
const (
	provisionPoll  = 20 * time.Second
	captureTimeout = 45 * time.Minute
	sealScriptPath = "/usr/local/sbin/rasputin-seal"
)
```

Change **only** line 23:

```go
	provisionPoll  = 5 * time.Second
```

(Keep the two-space alignment before `=`; `gofmt` keeps it because the const block is aligned as a group.)

`provisionPoll` is referenced in exactly one other place — bake.go line 207, inside `waitProvisioned`:

```go
		case <-time.After(provisionPoll):
```

— which needs no change. No test anywhere references `provisionPoll` (verified with `grep -rn provisionPoll --include='*.go'`: only bake.go:23 and bake.go:207).

## What the implementer must NOT do (B1)

- Do **NOT** restructure `waitProvisioned` (bake.go lines 179–211). Its shape is deliberate: each poll opens a **fresh** SSH connection via `c.Resolver.Connect`, which re-verifies the node's MAC every time (CLAUDE.md trap: **"Never trust a hostname"** — this cluster once had two nodes answering to `rasputin002`).
- Do **NOT** touch `captureTimeout` or `sealScriptPath` in the same const block.
- Do **NOT** touch `internal/provision/templates/provision.service.tmpl` or any other template (ExecStartPost ordering etc.) — Lane C owns `internal/provision`.
- Do **NOT** "optimize" by holding one SSH connection open across polls — the fresh-connect-per-poll behavior is what the 5 s figure was costed against, and a held connection would go stale across the builder's reboots.

## B1 Definition of Done

1. `git diff internal/cluster/bake.go` shows exactly one changed line: `20 * time.Second` → `5 * time.Second` on the `provisionPoll` line.
2. `cd /Users/ericovis/Code/rasputin && go test ./internal/cluster/` passes.
3. `cd /Users/ericovis/Code/rasputin && go test ./...` passes (darwin, no hardware).
4. `cd /Users/ericovis/Code/rasputin && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` passes.
5. Expected runtime effect (do not verify on hardware in this lane; recorded for the owner): bake's "provisioned" log line lands 0–5 s after the marker file is created instead of 0–20 s; mean bake time drops ~7.5 s from the measured 22m30s baseline.

---

# TASK B2 — flash idempotency guard + `-force`

## What and why

`rasputin bake` finishes by verifying the builder came back running the freshly baked golden build (bake.go line 144 calls `verifyClone`, which compares the node's build id to the meta's). So immediately after every bake, the builder **already runs** the exact golden build — yet `rasputin flash all` rewrites it anyway: a full needless wipe, 8,589,934,592 bytes written to the SD card at ~15.9 MB/s (~540 s of raw write inside a 10m28s–13m18s flash), pure card wear plus up to a full flash of wall clock whenever the builder is the slowest card in the parallel run.

Fix: in `flashOne`, after the MAC-verified connect and **before** arming the reflash flag or touching any host-key state, read the node's build id from `/etc/rasputin-release` and compare it to `out/meta/golden.json`'s `build_id`. On exact match: skip, log clearly, report success (`SKIP` in the summary). A new `-force` flag overrides. **On ANY doubt — file missing, unreadable, malformed, empty id, transport error — proceed with the flash** (fail open to flashing; the guard may only ever *prevent* work, never a needed flash).

**Flag spelling: use `-force` everywhere** — in the log message, usage text, and this document. That matches what `fs.PrintDefaults()` prints. (Go's `flag` package accepts `--force` too at runtime; do not document or print the two-dash form anywhere.)

## Ground truth for the two build ids (verified — do not invent paths)

**Node side.** The golden image's firstrun writes the marker. `/Users/ericovis/Code/rasputin/internal/provision/templates/firstrun.sh.tmpl` lines 142–147 (READ ONLY — Lane C owns this file):

```sh
cat > /etc/rasputin-release <<RELEASE
build_id=$build_id
base={{.BaseImage}}
prepared_at={{.PreparedAt}}
first_boot_at=$(date -Is 2>/dev/null || echo unknown)
RELEASE
```

The cluster package already has both the path constant and the parser, in `/Users/ericovis/Code/rasputin/internal/cluster/status.go` (same package — use them, do not redefine, do not edit status.go):

- Line 26: `const ReleaseFile = "/etc/rasputin-release"`
- Lines 100–108: `func buildIDFrom(release string) string` — extracts the `build_id=` line, returns `""` when absent.

Read it with the exact command shape status.go line 84 already uses (`s.BuildID = buildIDFrom(mustOutput(conn, "cat "+ReleaseFile+" 2>/dev/null || true"))`), so a missing file is a clean empty answer rather than a cat error: `"cat " + ReleaseFile + " 2>/dev/null || true"`. The file is mode 0644 (written by root with a plain `cat >`); **plain `conn.Output`, no sudo** — do not route this through `RemoteFile` (which uses `sudo -n cat`) and do not touch any sudo plumbing (CLAUDE.md trap: **"`sudo -k -S`: the `-k` is load-bearing"** — nothing in this task needs to go near `Push`/sudo password code).

**Host side.** `/Users/ericovis/Code/rasputin/internal/cluster/flash.go` lines 20–29 define the meta; the field is `BuildID` (JSON `build_id`) in `out/meta/golden.json`:

```go
type GoldenMeta struct {
	BuildID  string    `json:"build_id"`
	...
```

`flashOne` already receives `meta *GoldenMeta` (populated by `cluster.ReadGoldenMeta()` in `cmd/rasputin/flash.go` line 26). Compare `buildIDFrom(release)` against `meta.BuildID`. Note `meta` may be nil when called from other paths (verifyClone at flash.go line 136 already guards `meta != nil`) — the guard must be inert when `meta == nil` or `meta.BuildID == ""`.

Do **not** use any timestamp fields from `/etc/rasputin-release` (`first_boot_at` is written by a clockless Pi — CLAUDE.md trap: **"A Pi has no RTC"**). The comparison is `build_id` string equality, nothing else.

## Edits to `/Users/ericovis/Code/rasputin/internal/cluster/flash.go`

### (a) Add `Skipped` to `FlashResult` — anchor: lines 56–63

Current:

```go
// FlashResult is one node's outcome.
type FlashResult struct {
	Node     string
	Duration time.Duration
	BuildID  string
	Hostname string
	Err      error
}
```

Add a field (name it exactly `Skipped`):

```go
	// Skipped is set when the node already ran the golden build and was
	// left untouched. A skipped node is a success.
	Skipped bool
```

Do NOT change `OK()` (line 66: `func (r FlashResult) OK() bool { return r.Err == nil }`) — a skip has `Err == nil` and is therefore already OK.

### (b) Add `FlashOptions` and thread it through — anchors: lines 68–95

Current signatures:

```go
func (c *Cluster) Flash(ctx context.Context, srv *server.Server, img Image, meta *GoldenMeta, targets []config.Node) []FlashResult {
```
(line 73) and inside the goroutine, line 80:
```go
			results[i] = c.flashOne(ctx, srv, img, meta, node)
```
and line 87:
```go
func (c *Cluster) flashOne(ctx context.Context, srv *server.Server, img Image, meta *GoldenMeta, node config.Node) FlashResult {
```

Add, near `FlashResult`:

```go
// FlashOptions adjusts how a flash run treats its targets.
type FlashOptions struct {
	// Force reflashes a node even when it already runs the golden build.
	Force bool
}
```

and append `opts FlashOptions` as the final parameter of both `Flash` and `flashOne`, passing it through at line 80. The only external caller is `cmd/rasputin/flash.go` line 51 (edit (e) below) — verified by `grep -rn 'c.Flash\|flashOne' --include='*.go' .`: only cmd/rasputin/flash.go:51, internal/cluster/flash.go:80, internal/cluster/flash.go:87. Nothing else in the repo calls `c.Flash` or `flashOne`.

### (c) The guard itself — placement is the whole point

Anchor — flash.go lines 87–99 currently read:

```go
func (c *Cluster) flashOne(ctx context.Context, srv *server.Server, img Image, meta *GoldenMeta, node config.Node) FlashResult {
	start := time.Now()
	res := FlashResult{Node: node.Name}

	conn, err := c.Resolver.Connect(ctx, node)
	if err != nil {
		res.Err = err
		return res
	}

	url := srv.URLFor(img.Name)
	c.Log("%s: arming a reflash from %s", node.Name, url)
	if err := WriteFlag(conn, FlagReflash, url); err != nil {
```

Insert the guard **after** the `c.Resolver.Connect` error check (after line 95's closing `}`) and **before** `url := srv.URLFor(img.Name)` (line 97). Exact insertion:

```go
	// Idempotency guard: a node already running the exact golden build has
	// nothing to gain from a 13-minute rewrite of an identical card — and a
	// bake leaves the builder in exactly that state. Any doubt (no marker,
	// unreadable, malformed) falls through to flashing. This runs only after
	// Connect verified the MAC, and a skip must leave every piece of
	// host-key state alone: nothing is replaced, so the pinned key and the
	// operator's known_hosts stay valid.
	if !opts.Force && meta != nil && meta.BuildID != "" {
		release, err := conn.Output("cat " + ReleaseFile + " 2>/dev/null || true")
		if err == nil {
			if id := buildIDFrom(release); id != "" && id == meta.BuildID {
				conn.Close()
				c.Log("%s: already running golden build %s — skipping (use -force to reflash anyway)", node.Name, id)
				res.BuildID = id
				res.Skipped = true
				res.Duration = time.Since(start)
				return res
			}
		}
	}
```

Fail-open enumeration (all of these MUST fall through to a normal flash):
- `opts.Force` is true;
- `meta == nil` or `meta.BuildID == ""`;
- `conn.Output` returns an error (transport hiccup);
- the release body is empty or has no `build_id=` line (`buildIDFrom` returns `""`);
- the ids differ.

Only exact non-empty equality with `Force` unset skips.

### (d) Why the placement — the two traps, spelled out

**Trap 1 (CLAUDE.md: "Clear a pinned SSH host key only *after* the node is down" / `RebootOptions.ReplacesSystem`).** The reflash path at flash.go lines 105–109:

```go
	// The node is about to replace its whole filesystem, host keys and all;
	// ReplacesSystem drops the pinned key once it is down.
	timeout := time.Duration(c.Cfg.Timeouts.FlashMinutes) * time.Minute
	back, err := c.rebootWatchingProgress(ctx, node, conn, srv,
		RebootOptions{Back: timeout, ReplacesSystem: true})
```

drops the pinned host key (via `RebootAndWait` → `c.State.ForgetHostKey`, cluster.go lines 184–189) and, after verification, purges the operator's `~/.ssh/known_hosts` (flash.go line 124: `c.PurgeKnownHosts(node, back.Host())`). **A skipped node replaces nothing.** The skip return path must execute NONE of: `WriteFlag(conn, FlagReflash, ...)`, `Reboot`, `rebootWatchingProgress`, `ForgetHostKey`, `PurgeKnownHosts`. It closes the connection and returns. The insertion point above — before line 97 — guarantees this structurally; do NOT restructure the function so the skip shares any code after line 97, and do NOT add any "cleanup" of host keys or known_hosts to the skip path. If the pin or known_hosts were cleared on a skip, the very next real connection would blind-re-pin whatever answers — exactly the impersonation window the pin exists to close.

**Trap 2 (CLAUDE.md: "Never trust a hostname").** MAC verification lives *inside* `c.Resolver.Connect` (`internal/nodes/nodes.go` lines 186–227: every candidate connection runs `cat /sys/class/net/eth0/address` and refuses a mismatch, lines 204–216). The guard must run on the connection `Connect` returned — after lines 91–95, never before it, and never via a separate dial that bypasses the resolver. Do not weaken, reorder, or duplicate the MAC check.

Also do NOT touch: the reflash-flag semantics (`FlagReflash`), `verifyClone`, `waitSystemSettled`/`settlePoll`, or anything in `bake.go` beyond Task B1 — bake's own `bakeReflash` (bake.go line 156) must keep flashing the builder unconditionally (it flashes the *vanilla* image; the guard lives only in `flashOne`, which bake never calls).

### (e) Edits to `cmd/rasputin/flash.go` and `cmd/rasputin/main.go`

All anchors verified:

1. **Flag** — in `/Users/ericovis/Code/rasputin/cmd/rasputin/flash.go`, after line 15 `fs := flag.NewFlagSet("flash", flag.ContinueOnError)` add:

```go
	force := fs.Bool("force", false, "reflash a node even when it already runs the golden build")
```

2. **Usage** — flash.go lines 16–21 currently:

```go
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: rasputin flash <node...|all>\n\n"+
			"Reflashes each node from out/golden.img.zst and verifies it comes\n"+
			"back with the golden build id and its own hostname. Nodes are\n"+
			"flashed in parallel.\n")
	}
```

Change the first line to `"usage: rasputin flash [flags] <node...|all>\n\n"`, append sentences noting that a node already running the golden build is skipped unless `-force` is given and that flags must come **before** the node list, then end the format string with `"\nflags:\n"` and call `fs.PrintDefaults()` after the Fprintf — mirror `cmd/rasputin/adopt.go` lines 18–24, which is the house pattern (its Usage ends with `"...\n\nflags:\n"` then `fs.PrintDefaults()`). The flags-first note matters: Go's `flag` package stops parsing at the first non-flag argument, so `rasputin flash -force all` works but `rasputin flash all -force` would be rejected as an unknown node named `-force` by `Select`.

3. **Call site** — flash.go line 51 currently:

```go
	results := c.Flash(context.Background(), srv, cluster.GoldenImage, meta, targets)
```

becomes:

```go
	results := c.Flash(context.Background(), srv, cluster.GoldenImage, meta, targets, cluster.FlashOptions{Force: *force})
```

4. **Summary** — flash.go lines 53–63 currently:

```go
	fmt.Printf("\n%-14s %-8s %-10s %s\n", "NODE", "RESULT", "TIME", "DETAIL")
	var failed int
	for _, r := range results {
		if r.OK() {
			fmt.Printf("%-14s %-8s %-10s build %s\n", r.Node, "PASS",
				r.Duration.Round(time.Second), r.BuildID)
			continue
		}
		failed++
		fmt.Printf("%-14s %-8s %-10s %v\n", r.Node, "FAIL", r.Duration.Round(time.Second), r.Err)
	}
```

Inside the `r.OK()` branch, print `SKIP` instead of `PASS` for a skipped node, e.g.:

```go
		if r.OK() {
			verdict, detail := "PASS", "build "+r.BuildID
			if r.Skipped {
				verdict, detail = "SKIP", "already on build "+r.BuildID
			}
			fmt.Printf("%-14s %-8s %-10s %s\n", r.Node, verdict,
				r.Duration.Round(time.Second), detail)
			continue
		}
```

A skipped node must NOT increment `failed` (it doesn't, since `OK()` is true — do not change that) and the command must still exit 0 when every node either flashed or skipped.

5. **Top-level help** — `/Users/ericovis/Code/rasputin/cmd/rasputin/main.go` line 26 currently reads:

```go
	{"flash", "flash <node...|all>", "reflash node(s) from the golden image", runFlash},
```

Change **only** the usage string in this one entry to `"flash [flags] <node...|all>"`. Touch nothing else in main.go — no other command-table entry, no dispatch logic.

## Tests — `/Users/ericovis/Code/rasputin/internal/cluster/cluster_test.go`

Add the tests to `cluster_test.go` (same package `cluster`, so unexported `flashOne`, `settlePoll`, `buildIDFrom` are reachable). **Harness note:** the repo's only in-process SSH server (`internal/sshx/testserver_test.go`) is test-package-private and cannot be imported here; the cluster package's established hardware-free harness is a fake `nodes.Dialer` + fake `nodes.Conn` — copy the `reflashDialer`/`rebootConn`/`newRebootCluster` pattern from `/Users/ericovis/Code/rasputin/internal/cluster/reboot_test.go` (lines 18–124), which exercises the identical code paths *including real host-key pinning through `state.Store`*.

**Reuse the `rebootYAML` constant (reboot_test.go lines 18–31) — it is package-level and directly usable. `newRebootCluster` (reboot_test.go line 101) can NOT be reused: its signature is `newRebootCluster(t *testing.T, d *reflashDialer)`, hard-typed to `*reflashDialer`, so it will not accept your new `flashDialer`.** Write a parallel constructor in cluster_test.go instead — `newFlashCluster(t *testing.T, d *flashDialer) (*Cluster, config.Node)` — duplicating newRebootCluster's body: `config.Parse([]byte(rebootYAML), "test.yaml")`, `state.Load(filepath.Join(t.TempDir(), "state.json"))`, assign `d.store = st`, then a `&Cluster{Cfg: cfg, State: st, Log: func(string, ...any) {}, Resolver: &nodes.Resolver{Cfg: cfg, State: st, Dial: d, ARP: func() (map[string]string, error) { return nil, nil }, PollInterval: time.Millisecond, DialTimeout: time.Second}}`, returning `c, cfg.Nodes[0]`. Do NOT modify reboot_test.go. The node is `rasputin001`, MAC `b8:27:eb:01:02:03` — must match what `flashConn` answers, or `Connect` correctly refuses it.

**Safety rule for every test that can reach the end of `flashOne`:** call `t.Setenv("HOME", t.TempDir())` FIRST. `flashOne`'s success path runs `c.PurgeKnownHosts`, which shells out to `ssh-keygen -R` against `$HOME/.ssh/known_hosts` — without the sandbox, `go test` would mutate the developer's real known_hosts, which on the owner's Mac genuinely contains `rasputin*` entries (this is the pattern `knownhosts_test.go` lines 40 and 72 already use).

### Fake plumbing (reference implementation — adapt freely, keep the behaviors)

```go
// flashConn is a node as flashOne sees it. release is what its
// /etc/rasputin-release says; "" models a missing marker.
type flashConn struct {
	d       *flashDialer
	release string
}

func (c *flashConn) Run(cmd string) (sshx.Result, error) {
	switch {
	case cmd == "cat "+nodes.MACPath:
		return sshx.Result{Stdout: "b8:27:eb:01:02:03\n"}, nil
	case cmd == "cat "+ReleaseFile+" 2>/dev/null || true": // the guard's read
		return sshx.Result{Stdout: c.release}, nil
	case cmd == "cat "+ReleaseFile: // verifyClone's read
		if c.release == "" {
			return sshx.Result{}, fmt.Errorf("cat: %s: No such file or directory", ReleaseFile)
		}
		return sshx.Result{Stdout: c.release}, nil
	case cmd == "hostname":
		return sshx.Result{Stdout: "rasputin001\n"}, nil
	case cmd == "systemctl is-system-running":
		return sshx.Result{Stdout: "running\n"}, nil
	case strings.Contains(cmd, "reboot"):
		c.d.record("reboot")
		return sshx.Result{}, nil
	}
	return sshx.Result{}, nil
}
func (c *flashConn) Sudo(cmd string) (sshx.Result, error) { return c.Run("sudo -n " + cmd) }
func (c *flashConn) Output(cmd string) (string, error) {
	r, err := c.Run(cmd)
	return strings.TrimRight(r.Stdout, "\n "), err
}
func (c *flashConn) Push(path string, _ []byte, _ string) error { c.d.record("push:" + path); return nil }
func (c *flashConn) Fetch(string) ([]byte, error)               { return nil, nil }
func (c *flashConn) User() string                               { return "berry" }
func (c *flashConn) Host() string                               { return "192.168.0.74" }
func (c *flashConn) Close() error                               { return nil }

// flashDialer mirrors reboot_test.go's reflashDialer: pins the presented key
// on a fresh handshake, refuses a changed one, and swaps old→new identity
// (host key AND release contents) across the down window.
type flashDialer struct {
	mu                     sync.Mutex
	store                  *state.Store
	dials                  int
	downFrom, upFrom       int
	oldKey, newKey         string
	oldRelease, newRelease string
	events                 []string // "push:<path>", "reboot"
}

func (d *flashDialer) record(e string) { d.mu.Lock(); d.events = append(d.events, e); d.mu.Unlock() }

func (d *flashDialer) Dial(_ context.Context, node, host string) (nodes.Conn, error) {
	d.mu.Lock()
	d.dials++
	n := d.dials
	d.mu.Unlock()
	if n >= d.downFrom && n < d.upFrom {
		return nil, fmt.Errorf("no route to host")
	}
	key, release := d.oldKey, d.oldRelease
	if n >= d.upFrom {
		key, release = d.newKey, d.newRelease
	}
	if known := d.store.HostKey(node); known == "" {
		if err := d.store.SetHostKey(node, key); err != nil {
			return nil, err
		}
	} else if known != key {
		return nil, fmt.Errorf("host key for %s changed", node)
	}
	return &flashConn{d: d, release: release}, nil
}
```

HTTP server for the flash paths (flashOne calls `srv.URLFor` and the progress watcher calls `srv.ProgressFor`): start a real one — `server.Start(server.Options{Port: 0, BindIP: net.ParseIP("127.0.0.1"), OutDir: t.TempDir()})`, `defer srv.Close()`. (`BindIP` 127.0.0.1 is the documented test setting, server.go lines 37–39; no `Register` needed because the fake node never downloads.) For the pure-skip test `srv` is never dereferenced before the guard returns, but pass a real server anyway for uniformity.

Also set `settlePoll` to `time.Millisecond` with the save/restore pattern of reboot_test.go lines 235–237 (`old := settlePoll; settlePoll = time.Millisecond; t.Cleanup(func() { settlePoll = old })`) — harmless here since the fake answers `running` immediately, but it keeps the tests immune to future scripted states.

Dial phasing that avoids `waitGone`'s hardcoded 3 s sleep (cluster.go line 210: `case <-time.After(3 * time.Second):`): use `downFrom: 2, upFrom: 4`. Dial 1 is `flashOne`'s own connect (up, old identity); `waitGone`'s first poll then finds the node already gone (dials 2–3 fail regardless of whether `Connect` tries one or two candidates), so no sleep; `WaitFor` gets the node back at dial 4 with the new key and new release. Total test time well under a second.

Release fixtures — **these are function-scope declarations: put them inside each test function (or convert to package-level `const`/`var`; the `:=` form shown does not compile at package level):**

```go
const goldenID = "20260829T021418Z-66b2f9" // may be package-level

// Inside each test function:
goldenRelease := "build_id=" + goldenID + "\nbase=x.img\nprepared_at=2026-08-29\nfirst_boot_at=unknown\n"
staleRelease := "build_id=20260828T233619Z-40fd3e\nbase=x.img\nprepared_at=2026-08-28\nfirst_boot_at=unknown\n"
meta := &GoldenMeta{BuildID: goldenID}
```

### The five required tests

1. **`TestFlashSkipsANodeAlreadyOnTheGoldenBuild`** — dialer `downFrom: 1 << 30, upFrom: 1 << 30` (never goes down), `oldRelease: goldenRelease`, `oldKey: "ssh-ed25519 PINNED"`. Sandbox HOME and pre-create a known_hosts file:
   ```go
   home := t.TempDir()
   t.Setenv("HOME", home)
   if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil { t.Fatal(err) }
   khPath := filepath.Join(home, ".ssh", "known_hosts")
   before := []byte("rasputin001 ssh-ed25519 AAAA\n")
   if err := os.WriteFile(khPath, before, 0o600); err != nil { t.Fatal(err) }
   ```
   (The `MkdirAll` is required — `os.WriteFile` into a nonexistent `.ssh` directory fails; this mirrors knownhosts_test.go lines 52–58.) Call `res := c.flashOne(ctx, srv, GoldenImage, meta, node, FlashOptions{})`. Assert ALL of:
   - `res.OK()` is true and `res.Skipped` is true; `res.BuildID == goldenID`.
   - No reflash was armed and no reboot issued: `d.events` contains no `"push:"+FlagReflash` and no `"reboot"` entry.
   - **Host-key pin untouched on skip** (the trap): `c.State.HostKey("rasputin001") == "ssh-ed25519 PINNED"` after the call.
   - known_hosts untouched: re-read `khPath` after the call and compare full contents byte-for-byte against `before`.

2. **`TestFlashProceedsOnBuildMismatch`** — dialer `downFrom: 2, upFrom: 4`, `oldRelease: staleRelease`, `newRelease: goldenRelease`, `oldKey != newKey`. Sandbox HOME. Assert: `res.OK()` true, `res.Skipped` false, `res.BuildID == goldenID`; `d.events` contains `"push:"+FlagReflash` and `"reboot"`; and the pin was rotated to the NEW key (`c.State.HostKey("rasputin001") == newKey`) — proving the `ReplacesSystem` path still fired for a real flash.

3. **`TestFlashProceedsWhenTheReleaseMarkerIsMissing`** — same as (2) but `oldRelease: ""` (guard's `|| true` read comes back empty → `buildIDFrom` returns `""` → fail open). Assert flash proceeded (`"push:"+FlagReflash` present, `res.Skipped` false, `res.OK()` true).

4. **`TestFlashForceOverridesTheSkip`** — same phased dialer as (2) (`downFrom: 2, upFrom: 4`, `oldKey != newKey`) but `oldRelease: goldenRelease` **and** `newRelease: goldenRelease` (node already golden, comes back golden), called with `FlashOptions{Force: true}`. Assert the flash proceeded anyway: `res.Skipped` false, `res.OK()` true, `d.events` contains `"push:"+FlagReflash`.

5. **`TestFlashGuardFailsOpenWithoutMeta`** — **MUST use the phased dialer, exactly as (4): `downFrom: 2, upFrom: 4`, `oldRelease: goldenRelease`, `newRelease: goldenRelease`, `oldKey != newKey`.** Do NOT use a never-down dialer here: with `meta == nil` the guard is inert, so `WriteFlag` + reboot run for real, and against a node that never goes down `waitGone` (cluster.go lines 195–213) would spin on its 3 s sleep until the 3-minute `GoneTimeout` (cluster.go line 44) — a passing-but-3-minute test. Sandbox HOME. Call `res := c.flashOne(ctx, srv, GoldenImage, nil /* meta */, node, FlashOptions{})`. With `meta == nil`, `verifyClone` skips the id comparison (flash.go line 136 guards `meta != nil`), so the whole path completes green. Assert: `res.Skipped == false` and `d.events` contains `"push:"+FlagReflash` (the guard did not skip).

Keep test 1's known_hosts assertion strict: read the file before and after and compare full contents.

## What the implementer must NOT do (B2) — trap checklist

- **"Clear a pinned SSH host key only *after* the node is down" (`RebootOptions.ReplacesSystem`)**: the skip path must not call `ForgetHostKey`, `PurgeKnownHosts`, or `ssh-keygen` in any form, and must return before `WriteFlag`/`rebootWatchingProgress`. Do not refactor `RebootAndWait`/`waitGone` (cluster.go lines 173–213) — the ordering there is a fixed regression (`TestRebootReplacingSystemClearsTheStaleHostKey`).
- **"Never trust a hostname"**: the guard runs only on the `c.Resolver.Connect`-returned connection (MAC already verified inside `Connect`, nodes.go lines 204–216). No separate dial, no comparison keyed on hostname, no weakening of the MAC check.
- **"A Pi has no RTC"**: decide on `build_id` equality only; ignore `first_boot_at`/`prepared_at` in the release file.
- **"`sudo -k -S`: the `-k` is load-bearing"**: read the release with plain `conn.Output` (`cat`, no sudo); touch nothing in `internal/sshx` or the sudo-password plumbing.
- Do not add the guard to `bakeReflash` or `Dryrun`; do not change `verifyClone`, `Reboot`, `WriteFlag`, or `FlagReflash`.
- Do not modify `internal/cluster/status.go`, `internal/cluster/reboot_test.go`, `internal/cluster/knownhosts_test.go`, `internal/provision/**`, `internal/sshx/**`, or `internal/state/**` (other lanes / shared).
- In `cmd/rasputin/main.go`, change only the flash entry's usage string (line 26); nothing else.
- Tests must not touch the real `$HOME`, must not open real network listeners other than the 127.0.0.1 test server, and must not need hardware.

## B2 Definition of Done

1. `internal/cluster/flash.go` compiles with `FlashOptions{Force bool}`, `FlashResult.Skipped`, and the guard inserted between the `Connect` error check and `url := srv.URLFor(img.Name)`; nothing after that point changed.
2. `cmd/rasputin/flash.go` wires `-force` (registered before `fs.Parse`, documented in usage with `fs.PrintDefaults()` and a note that flags precede the node list), passes `cluster.FlashOptions{Force: *force}` at the `c.Flash` call, and prints `SKIP` + `already on build <id>` for skipped nodes without counting them as failures. `cmd/rasputin/main.go`'s flash table entry reads `"flash [flags] <node...|all>"` and nothing else in main.go changed.
3. All five new tests exist in `/Users/ericovis/Code/rasputin/internal/cluster/cluster_test.go` and pass: `cd /Users/ericovis/Code/rasputin && go test ./internal/cluster/ -run 'TestFlash' -v` — expected wall clock for the five combined: **< 5 s** (no 3-second `waitGone` sleep; if you see a test taking ≥ 3 s, your dial phasing is wrong — every test whose flash proceeds must use `downFrom: 2, upFrom: 4`).
4. Pre-existing regression tests still pass, notably `TestRebootReplacingSystemClearsTheStaleHostKey`, `TestRebootPreservingSystemKeepsTheHostKey`, `TestPurgeKnownHostsIsBestEffort`: `go test ./internal/cluster/`.
5. `cd /Users/ericovis/Code/rasputin && go test ./...` passes on darwin with no hardware.
6. `cd /Users/ericovis/Code/rasputin && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` passes.
7. `cd /Users/ericovis/Code/rasputin && make test` passes (it is exactly steps 5+6).
8. `go vet ./...` is clean; `gofmt -l internal/cluster cmd/rasputin` prints nothing.
9. Expected operational outcomes (for the owner to confirm on hardware later, NOT part of this lane's execution): after a bake, `rasputin flash all` shows the builder as `SKIP already on build 20260829T021418Z-66b2f9` within seconds (one SSH round-trip, < 1 s decision) while the other three flash normally (~10m28s–13m18s each, in parallel); exit code 0. Per skipped node: 8,589,934,592 bytes NOT written to its SD card (~540 s of raw card writing at ~15.9 MB/s avoided), and total wall clock shrinks by up to one full flash whenever the builder would have been the slowest card. `rasputin flash -force all` rewrites all four.
10. Commit locally with a message naming both tasks; never push; `out/` and `cache/` stay uncommitted.