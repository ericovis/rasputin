# Phase 3 — orchestration (T12–T15)

## T12 — HTTP server `internal/server` [P]B

- `Start(cfg) (*Server, error)`: bind on the LAN IP (find it: dial udp
  8.8.8.8:53, LocalAddr — no packet is sent; error out if it's loopback),
  cfg port (0 → random). Expose `BaseURL()`.
- `GET /i/<name>` serves files registered from out/ (golden.img.zst,
  vanilla-custom.img.zst). Wrap the response writer to count bytes per
  remote IP + per ?mac= param; expose `Progress()` snapshots (CLI polls this
  to render "node X: 43% of image downloaded").
- `POST /capture?id=<buildid>&mac=<mac>`: stream body to
  `out/incoming-<id>.zst.tmp`; on clean EOF fsync + rename to the path the
  bake command registered; 500 + delete on error; single-flight per id.
  Respond 200 only after rename. Also record sha256 while streaming.
- Unit tests with httptest + real TCP listener on 127.0.0.1 (skip LAN-IP
  detection in tests via cfg override).

VERIFY:
```
go test ./internal/server/ -v | grep -q PASS
```

## T13 — SSH client + node resolution + state cache [P]B

- `internal/sshx`: dial with key from cfg (parse once), try cfg.ssh.users in
  order until auth succeeds; `Run(cmd) (stdout, stderr, err)`,
  `Sudo(cmd)` (prefix `sudo -n`), `Push(path, data, mode)` (via
  `sudo -n tee`, not sftp — simpler). HostKeyCallback: accept-and-record into
  `out/state.json` per node+generation; flashing resets the recorded key
  (nodes regenerate host keys by design — document this tradeoff in README).
  10s dial timeout.
- `internal/nodes`: resolve a CLI arg (name | mac | ip | `all`) against YAML
  → []Node. IP lookup order: (1) `out/state.json` cached ip if it still
  answers SSH and `cat /sys/class/net/eth0/address` matches the node's MAC —
  ALWAYS verify MAC after connecting, mDNS names have collided on this
  cluster before; (2) `<name>.local` mDNS; (3) arp scan: ping broadcast then
  parse `arp -a` for the MAC (darwin format). Persist last-known ip+time in
  out/state.json on every successful contact.
- `Preflight(node)`: uname -m == aarch64, /boot/firmware is a mountpoint,
  `sudo -n true` works, boot partition free space > 2× recovery.gz size.
  Returns structured result (used by adopt + T16).

VERIFY:
```
go test ./internal/sshx/ ./internal/nodes/ | grep -c ok | grep -q 2
# resolution tests use a fake dialer interface; no hardware needed
```

## T14 — `adopt` + `dryrun` commands

- `adopt <node|all>`: for each node (sequential): Preflight; back up
  `config.txt` → `config.txt.pre-rasputin` (only if backup absent); push
  out/recovery.gz → /boot/firmware/recovery.gz; push nodes.conf; patch
  config.txt ON the node via the same PatchConfigTxt logic (fetch, patch
  locally, push back); `sync`. Then `--reboot-check` (default on): reboot
  node, poll SSH until back (10 min timeout), assert
  `grep -q 'initramfs recovery.gz' /boot/firmware/config.txt` and dmesg has
  no rasputin errors. Print per-node PASS/FAIL. Adopt does NOT flash.
- `dryrun <node|all>`: ensure server running with the image registered
  (vanilla-custom.img.zst if no golden yet, else golden); write flag file
  `reflash-dryrun` with first line = server URL (via Sudo tee); reboot; poll
  SSH back (15 min); fetch and print /boot/firmware/reflash-dryrun.log;
  PASS iff it contains `result: OK`. Server progress is printed while waiting
  ("node pulled N MiB").

VERIFY:
```
go build ./... && go test ./...
go run ./cmd/rasputin adopt --help && go run ./cmd/rasputin dryrun --help
# hardware execution happens in phase 4, not here
```

## T15 — `flash`, `bake`, `status`

- `flash <node...|all>`: requires out/golden.img.zst + out/meta/golden.json.
  All named nodes IN PARALLEL (goroutine per node, prefixed output lines):
  write `reflash` flag (line 1 = server golden URL), reboot, then poll:
  phase1 "downloading" (server progress by mac), phase2 "flashing/rebooting"
  (ssh unreachable), phase3 ssh back → assert /etc/rasputin-release build_id
  == golden meta build_id AND hostname == node name AND
  `systemctl is-system-running` in (running|degraded) — then report per-node
  total duration. Timeout cfg.timeouts.flash_minutes → mark node FAILED,
  keep others going, exit nonzero if any failed.
- `bake`: orchestrate on cfg.builder: (1) ensure prepare artifacts; (2) adopt
  builder if not adopted; (3) reflash builder with VANILLA image (flag =
  vanilla URL); (4) poll until SSH back as provision user AND
  /var/lib/rasputin/provisioned exists (bake_minutes timeout; surface
  provision journal tail on timeout); (5) `sudo rasputin-seal`; (6) write
  `capture` flag (line 1 = POST URL with new build id), reboot; (7) wait for
  capture upload complete (server); (8) verify: zstd-decode the upload
  counting bytes (must equal mbr.UsedBytes of decoded stream? — decode
  header+MBR only, then full decode to /dev/null verifying CRC), write
  out/meta/golden.json {build_id, sha256, bytes, base, baked_at}; rename to
  out/golden.img.zst; (9) builder reboots into normal sealed-then-healed
  boot; wait SSH, verify hostname + build_id. Print total bake time.
- `status`: table for all YAML nodes: name, mac, ip(source), ssh ok(as which
  user), hostname, build_id (or "stock"), uptime, provisioned?. Parallel
  collection, 5s timeouts, never modifies anything.

VERIFY:
```
go build ./... && go vet ./... && go test ./...
go run ./cmd/rasputin flash --help; go run ./cmd/rasputin bake --help
go run ./cmd/rasputin status        # against live cluster: must print 4 rows, ≥2 reachable, and NOT touch anything
```
