# Phase 0 — foundation (T01–T04)

## T01 — Module scaffold, YAML config, .gitignore

Create:

- `go.mod`: module `github.com/ericovis/rasputin`, go 1.27. `go get`:
  gopkg.in/yaml.v3, golang.org/x/crypto/ssh, github.com/klauspost/compress,
  github.com/ulikunitz/xz, github.com/diskfs/go-diskfs,
  github.com/insomniacslk/dhcp, github.com/vishvananda/netlink.
- `.gitignore`: `out/`, `cache/`, `*.img`, `*.img.zst`, `*.img.xz`, `recovery.gz`.
- `Makefile` targets: `build` (agent → embed → CLI, see T04), `test`
  (`go test ./...` and `GOOS=linux GOARCH=arm64 go build ./...`), `clean`.
- `internal/config/config.go` + `config_test.go`: load/validate `rasputin.yaml`.
  Struct mirrors the YAML below. Validation: ≥1 node, MACs parse
  (net.ParseMAC, normalize to lowercase colon form), builder is a known node
  name, user non-empty, port 1–65535, expand `~` in key paths. Defaults:
  port 8080, rootfs_size_gb 8, timeouts as below.
- `rasputin.yaml` (repo root — this exact content is the live config):

```yaml
cluster: rasputin
server:
  port: 8080            # 0 = random free port; bind IP is always auto-detected
image:
  source_url: https://downloads.raspberrypi.com/raspios_lite_arm64_latest
  rootfs_size_gb: 8     # firstrun grows rootfs to this cap (fits 32GB cards, keeps captures small)
ssh:
  key: ~/.ssh/id_ed25519
  users: [berry, ericovis]   # tried in order when connecting
provision:
  user: berry
  authorized_keys: ~/.ssh/id_ed25519.pub
  timezone: America/Sao_Paulo
  locale: en_US.UTF-8
  packages: [podman, curl, htop, vim, git, tmux]
builder: rasputin001
timeouts:
  flash_minutes: 25
  bake_minutes: 45
nodes:
  - { name: rasputin001, mac: "b8:27:eb:01:02:03" }
  - { name: rasputin002, mac: "b8:27:eb:04:05:06" }
  - { name: rasputin003, mac: "b8:27:eb:07:08:09" }
  - { name: rasputin004, mac: "b8:27:eb:0a:0b:0c" }
```

- `cmd/rasputin/main.go`: flag `-c` (default `./rasputin.yaml`), subcommand
  dispatch (stdlib only, no cobra), commands stubbed with "not implemented"
  errors; `prepare adopt dryrun bake flash status serve` listed in help.

VERIFY:
```
go build ./... && go vet ./... && go test ./...
go run ./cmd/rasputin -c rasputin.yaml status 2>&1 | grep -qi 'not implemented'
git check-ignore out/x cache/x
```

## T02 — cpio newc writer package `internal/cpio` [P]A

Small writer, no deps: `NewWriter(io.Writer)`, `WriteFile(name string, mode
os.FileMode, data []byte)`, `WriteDir(name)`, `WriteCharDev(name string, mode
os.FileMode, major, minor int)`, `Close()` (writes `TRAILER!!!`). newc format:
110-byte ASCII header per FACTS.md; 4-byte alignment padding after name and
after data. Test: golden test crafting `dev/console` and comparing against the
byte layout documented in FACTS.md; round-trip test extracting with the
system's `cpio -t` is NOT required (darwin cpio differs) — instead write a
minimal newc *reader* in the test file to verify structure.

VERIFY:
```
go test ./internal/cpio/ -v -run . | grep -q PASS
```

## T03 — kmsg logger + MBR parser [P]A

- `internal/kmsg`: `Logger` writing `rasputin: ...` lines to an io.Writer
  (agent passes /dev/kmsg; tests pass a buffer). Also mirrors to stdout.
  Linux-only file opening lives behind build tags; the formatting logic is
  portable and tested on darwin.
- `internal/mbr`: `UsedBytes(r io.ReaderAt) (int64, error)` — read sector 0,
  check 0x55AA signature, parse the 4 partition entries (LBA start uint32 LE
  at offset 446+16i+8, sector count at +12), return
  `512 * max(start+count)` over non-empty entries. Reject GPT (type 0xEE).
  Test with a synthetic 512-byte MBR fixture matching the stock Pi layout.

VERIFY:
```
go test ./internal/kmsg/ ./internal/mbr/ | grep -c ok | grep -q 2
```

## T04 — initramfs assembler `internal/initramfs`

Depends on T02 and T05 (needs the agent package to exist to build it).

- `Build(cfg, outPath string) error`:
  1. `go build` the agent: env `CGO_ENABLED=0 GOOS=linux GOARCH=arm64`,
     `-trimpath -ldflags "-s -w -X main.defaultURL=<cfg default URL>"`,
     package `./cmd/agent`, into a temp file. Invoke the `go` binary via
     os/exec (the tool is always run from the repo; document that).
  2. Verify with debug/elf: EM_AARCH64, no PT_INTERP, and warn if >12MB.
  3. Pack with internal/cpio: `dev` dir, `dev/console` (c 5:1 0600), `init`
     (the agent, mode 0755). gzip -9 (compress/gzip BestCompression) → outPath.
- The default URL baked in is `http://<placeholder>:port/golden.img.zst` —
  but since bind IP is dynamic, bake ldflag value from YAML only if the owner
  later pins one; empty default = agent requires URL from flag file and says
  so on kmsg. (reflash.sh-style manual `touch` trigger then needs a served
  default; document in README task.)
- Makefile `build` target: runs this via `go run ./cmd/rasputin prepare
  --initramfs-only` OR a tiny `cmd/mkinitramfs`; choose one and keep it.

VERIFY:
```
make build
go test ./internal/initramfs/   # includes a test that Build() output: gunzip+cpio-parse (use the T02 test reader), finds /init ELF aarch64 static and dev/console c 5:1
ls -l out/recovery.gz
```
