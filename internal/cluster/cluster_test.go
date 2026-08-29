package cluster

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/mbr"
	"github.com/ericovis/rasputin/internal/nodes"
	"github.com/ericovis/rasputin/internal/server"
	"github.com/ericovis/rasputin/internal/sshx"
	"github.com/ericovis/rasputin/internal/state"
)

func TestStatusTable(t *testing.T) {
	rows := []Status{
		{Name: "rasputin001", MAC: "b8:27:eb:01:02:03", IP: "192.168.0.74", Reachable: true,
			SSHUser: "berry", Hostname: "rasputin001", BuildID: "20260828T1-abc",
			Provisioned: true, Uptime: "up 2 hours"},
		{Name: "rasputin003", MAC: "b8:27:eb:07:08:09", Reachable: false,
			Err: fmt.Errorf("no route to host")},
	}
	out := StatusTable(rows)
	for _, want := range []string{
		"NODE", "rasputin001", "192.168.0.74", "berry", "20260828T1-abc", "yes", "up 2 hours",
		"rasputin003", "down",
		"rasputin003: no route to host",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table is missing %q:\n%s", want, out)
		}
	}
	// An unreachable node must not claim to be unprovisioned; it is unknown.
	lines := strings.Split(out, "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "rasputin003 ") && strings.Contains(l, "no  ") {
			t.Errorf("an unreachable node reported a provisioning state: %q", l)
		}
	}
}

func TestStatusTableMarksAnAdoptedStockNode(t *testing.T) {
	out := StatusTable([]Status{{Name: "n", Reachable: true, SSHUser: "berry", Adopted: true}})
	if !strings.Contains(out, "stock (adopted)") {
		t.Errorf("table = %s", out)
	}
	plain := StatusTable([]Status{{Name: "n", Reachable: true, SSHUser: "berry"}})
	if !strings.Contains(plain, "stock") || strings.Contains(plain, "adopted") {
		t.Errorf("table = %s", plain)
	}
}

func TestBuildIDFrom(t *testing.T) {
	release := "build_id=20260828T233619Z-40fd3e\nbase=2026-06-18-raspios.img\nbaked_at=2026-08-28\n"
	if got := buildIDFrom(release); got != "20260828T233619Z-40fd3e" {
		t.Errorf("buildIDFrom = %q", got)
	}
	if got := buildIDFrom("nothing here\n"); got != "" {
		t.Errorf("buildIDFrom = %q, want empty", got)
	}
}

func TestGoldenMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	if _, err := ReadGoldenMeta(); err == nil {
		t.Error("ReadGoldenMeta accepted a missing file")
	}
	want := &GoldenMeta{
		BuildID: "20260828T1-abc", SHA256: "deadbeef", Bytes: 12345,
		Base: "base.img", Builder: "rasputin001", BakedAt: time.Now().UTC().Truncate(time.Second),
		CardUsed: 999,
	}
	if err := WriteGoldenMeta(want); err != nil {
		t.Fatalf("WriteGoldenMeta: %v", err)
	}
	got, err := ReadGoldenMeta()
	if err != nil {
		t.Fatalf("ReadGoldenMeta: %v", err)
	}
	if got.BuildID != want.BuildID || got.Bytes != want.Bytes || got.CardUsed != want.CardUsed {
		t.Errorf("meta = %+v, want %+v", got, want)
	}

	if err := os.WriteFile(GoldenMetaPath, []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGoldenMeta(); err == nil {
		t.Error("ReadGoldenMeta accepted invalid JSON")
	}
}

// fakeCard builds a zstd-compressed disk image whose MBR claims exactly the
// bytes it contains, the way a real capture does.
func fakeCard(t *testing.T, p2Sectors uint32) []byte {
	t.Helper()
	const p1Start, p1Len = 8192, 1024
	sector := make([]byte, mbr.SectorSize)
	putEntry(sector, 0, 0x0c, p1Start, p1Len)
	putEntry(sector, 1, 0x83, p1Start+p1Len, p2Sectors)
	sector[510], sector[511] = 0x55, 0xAA

	total := int(p1Start+p1Len+p2Sectors) * mbr.SectorSize
	card := make([]byte, total)
	copy(card, sector)

	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(card); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func putEntry(s []byte, i int, ptype byte, start, count uint32) {
	e := s[446+i*16:]
	e[4] = ptype
	putLE32(e[8:12], start)
	putLE32(e[12:16], count)
}

func putLE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func TestVerifyImageAcceptsAGoodCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "golden.img.zst")
	if err := os.WriteFile(path, fakeCard(t, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	used, err := verifyImage(path)
	if err != nil {
		t.Fatalf("verifyImage: %v", err)
	}
	if want := int64(8192+1024+4096) * mbr.SectorSize; used != want {
		t.Errorf("used = %d, want %d", used, want)
	}
}

func TestVerifyImageRejectsCorruption(t *testing.T) {
	good := fakeCard(t, 4096)
	corrupt := append([]byte(nil), good...)
	corrupt[len(corrupt)-3] ^= 0xff // break the frame checksum

	dir := t.TempDir()
	cases := map[string][]byte{
		"corrupt":   corrupt,
		"truncated": good[:len(good)/2],
		"not zstd":  []byte("this is not an image at all"),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".zst")
			if err := os.WriteFile(path, content, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := verifyImage(path); err == nil {
				t.Fatal("verifyImage accepted an unusable image")
			}
		})
	}
	if _, err := verifyImage(filepath.Join(dir, "absent.zst")); err == nil {
		t.Error("verifyImage accepted a missing file")
	}
}

func TestVerifyImageRejectsASizeMismatch(t *testing.T) {
	// A card whose stream is longer than its partition table claims: either
	// the capture over-read or the table is wrong. Either way, not golden.
	const p2 = 4096
	card := fakeCard(t, p2)
	dec, err := zstd.NewReader(bytes.NewReader(card))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readAllAndClose(dec)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, make([]byte, mbr.SectorSize)...) // one sector too many

	var buf bytes.Buffer
	enc, _ := zstd.NewWriter(&buf, zstd.WithEncoderCRC(true))
	enc.Write(raw)
	enc.Close()

	path := filepath.Join(t.TempDir(), "long.zst")
	os.WriteFile(path, buf.Bytes(), 0o644)
	if _, err := verifyImage(path); err == nil {
		t.Fatal("verifyImage accepted an image longer than its partition table")
	}
}

func TestBestImagePrefersGolden(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	if _, err := BestImage(); err == nil {
		t.Error("BestImage found something in an empty directory")
	}

	os.MkdirAll("out", 0o755)
	os.WriteFile(VanillaImage.Path, []byte("vanilla"), 0o644)
	img, err := BestImage()
	if err != nil {
		t.Fatal(err)
	}
	if img.Name != VanillaImage.Name {
		t.Errorf("BestImage = %s, want the prepared image", img.Name)
	}

	os.WriteFile(GoldenImage.Path, []byte("golden"), 0o644)
	if img, _ = BestImage(); img.Name != GoldenImage.Name {
		t.Errorf("BestImage = %s, want the golden image", img.Name)
	}

	// An empty file is not an image.
	os.WriteFile(GoldenImage.Path, nil, 0o644)
	if img, _ = BestImage(); img.Name != VanillaImage.Name {
		t.Errorf("BestImage = %s, want the empty golden image ignored", img.Name)
	}
}

func TestProgressLine(t *testing.T) {
	p := server.Progress{
		Bytes: 100 << 20, Total: 700 << 20,
		Started: time.Now().Add(-10 * time.Second), Updated: time.Now(),
	}
	got := ProgressLine(p)
	for _, want := range []string{"100/700 MiB", "14%", "MB/s"} {
		if !strings.Contains(got, want) {
			t.Errorf("ProgressLine = %q, want it to contain %q", got, want)
		}
	}
	if got := ProgressLine(server.Progress{Bytes: 5 << 20}); !strings.Contains(got, "5 MiB") {
		t.Errorf("ProgressLine with no total = %q", got)
	}
}

func TestIsDisconnect(t *testing.T) {
	for _, s := range []string{"EOF", "connection reset by peer", "use of closed network connection", "broken pipe"} {
		if !isDisconnect(fmt.Errorf("%s", s)) {
			t.Errorf("%q should read as a disconnect", s)
		}
	}
	if isDisconnect(fmt.Errorf("permission denied")) {
		t.Error("permission denied should not read as a disconnect")
	}
}

func chdir(t *testing.T, dir string) func() {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { os.Chdir(old) }
}

func readAllAndClose(dec *zstd.Decoder) ([]byte, error) {
	defer dec.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(dec); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --- flash idempotency guard -------------------------------------------
//
// The fakes below mirror reboot_test.go's reflashDialer/rebootConn, with two
// additions the guard needs: the node answers with a /etc/rasputin-release
// body, and the dialer swaps that body (as well as the host key) across the
// down window, the way a real reflash does.

const goldenID = "20260829T021418Z-66b2f9"

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
func (c *flashConn) Push(path string, _ []byte, _ string) error {
	c.d.record("push:" + path)
	return nil
}
func (c *flashConn) Fetch(string) ([]byte, error) { return nil, nil }
func (c *flashConn) User() string                 { return "berry" }
func (c *flashConn) Host() string                 { return "192.168.0.74" }
func (c *flashConn) Close() error                 { return nil }

// flashDialer mirrors reboot_test.go's reflashDialer: pins the presented key
// on a fresh handshake, refuses a changed one, and swaps old->new identity
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

func (d *flashDialer) seen(e string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, got := range d.events {
		if got == e {
			return true
		}
	}
	return false
}

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
	// This is what sshx's HostKeyCallback does on every handshake.
	if known := d.store.HostKey(node); known == "" {
		if err := d.store.SetHostKey(node, key); err != nil {
			return nil, err
		}
	} else if known != key {
		return nil, fmt.Errorf("host key for %s changed", node)
	}
	return &flashConn{d: d, release: release}, nil
}

// newFlashCluster is newRebootCluster for the flash fakes; the constructor
// there is hard-typed to *reflashDialer and cannot be reused.
func newFlashCluster(t *testing.T, d *flashDialer) (*Cluster, config.Node) {
	t.Helper()
	cfg, err := config.Parse([]byte(rebootYAML), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	d.store = st
	c := &Cluster{
		Cfg:   cfg,
		State: st,
		Log:   func(string, ...any) {},
		Resolver: &nodes.Resolver{
			Cfg: cfg, State: st, Dial: d,
			ARP:          func() (map[string]string, error) { return nil, nil },
			PollInterval: time.Millisecond,
			DialTimeout:  time.Second,
		},
	}
	return c, cfg.Nodes[0]
}

// flashTestServer is a real HTTP server on loopback: flashOne asks it for the
// image URL and the progress watcher polls it. The fake node never downloads.
func flashTestServer(t *testing.T) *server.Server {
	t.Helper()
	srv, err := server.Start(server.Options{Port: 0, BindIP: net.ParseIP("127.0.0.1"), OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// sandboxHome points $HOME at a temp dir so PurgeKnownHosts' `ssh-keygen -R`
// can never touch the developer's real known_hosts, which on this cluster's
// owner's Mac genuinely contains rasputin* entries. It also shortens the
// settle poll so the retry logic runs without waiting on a real boot.
func sandboxHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	old := settlePoll
	settlePoll = time.Millisecond
	t.Cleanup(func() { settlePoll = old })
	return home
}

func goldenReleaseBody() string {
	return "build_id=" + goldenID + "\nbase=x.img\nprepared_at=2026-08-29\nfirst_boot_at=unknown\n"
}

func staleReleaseBody() string {
	return "build_id=20260828T233619Z-40fd3e\nbase=x.img\nprepared_at=2026-08-28\nfirst_boot_at=unknown\n"
}

// TestFlashSkipsANodeAlreadyOnTheGoldenBuild is the point of the guard: a
// bake leaves the builder already running the golden build, and `flash all`
// used to rewrite its card anyway. A skip must also leave every piece of
// host-key state alone — nothing was replaced, so the pin and the operator's
// known_hosts are both still correct.
func TestFlashSkipsANodeAlreadyOnTheGoldenBuild(t *testing.T) {
	home := sandboxHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	khPath := filepath.Join(home, ".ssh", "known_hosts")
	before := []byte("rasputin001 ssh-ed25519 AAAA\n")
	if err := os.WriteFile(khPath, before, 0o600); err != nil {
		t.Fatal(err)
	}

	d := &flashDialer{
		downFrom: 1 << 30, upFrom: 1 << 30,
		oldKey: "ssh-ed25519 PINNED", newKey: "ssh-ed25519 PINNED",
		oldRelease: goldenReleaseBody(), newRelease: goldenReleaseBody(),
	}
	c, node := newFlashCluster(t, d)
	meta := &GoldenMeta{BuildID: goldenID}

	res := c.flashOne(context.Background(), flashTestServer(t), GoldenImage, meta, node, FlashOptions{})

	if !res.OK() || !res.Skipped {
		t.Fatalf("result = %+v, want a skipped success", res)
	}
	if res.BuildID != goldenID {
		t.Errorf("BuildID = %q, want %q", res.BuildID, goldenID)
	}
	if d.seen("push:" + FlagReflash) {
		t.Error("a skipped node was armed with the reflash flag")
	}
	if d.seen("reboot") {
		t.Error("a skipped node was rebooted")
	}
	if got := c.State.HostKey("rasputin001"); got != "ssh-ed25519 PINNED" {
		t.Errorf("pinned key = %q, want it untouched — a skip replaces nothing, so "+
			"clearing the pin would let the next connection blind-re-pin whatever answers", got)
	}
	after, err := os.ReadFile(khPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Errorf("known_hosts = %q, want it untouched (%q)", after, before)
	}
}

// TestFlashProceedsOnBuildMismatch proves the guard only ever prevents
// needless work: a node on a different build is flashed as before, host-key
// rotation and all.
func TestFlashProceedsOnBuildMismatch(t *testing.T) {
	sandboxHome(t)
	d := &flashDialer{
		downFrom: 2, upFrom: 4,
		oldKey: "ssh-ed25519 OLDKEY", newKey: "ssh-ed25519 NEWKEY",
		oldRelease: staleReleaseBody(), newRelease: goldenReleaseBody(),
	}
	c, node := newFlashCluster(t, d)
	meta := &GoldenMeta{BuildID: goldenID}

	res := c.flashOne(context.Background(), flashTestServer(t), GoldenImage, meta, node, FlashOptions{})

	if !res.OK() {
		t.Fatalf("flashOne: %v", res.Err)
	}
	if res.Skipped {
		t.Error("a node on a stale build was skipped")
	}
	if res.BuildID != goldenID {
		t.Errorf("BuildID = %q, want %q", res.BuildID, goldenID)
	}
	if !d.seen("push:" + FlagReflash) {
		t.Error("no reflash flag was armed")
	}
	if !d.seen("reboot") {
		t.Error("the node was never rebooted")
	}
	if got := c.State.HostKey("rasputin001"); got != d.newKey {
		t.Errorf("pinned key = %q, want the new key %q — the ReplacesSystem path did not fire", got, d.newKey)
	}
}

// TestFlashProceedsWhenTheReleaseMarkerIsMissing pins the fail-open rule: any
// doubt about what a node is running means flashing it.
func TestFlashProceedsWhenTheReleaseMarkerIsMissing(t *testing.T) {
	sandboxHome(t)
	d := &flashDialer{
		downFrom: 2, upFrom: 4,
		oldKey: "ssh-ed25519 OLDKEY", newKey: "ssh-ed25519 NEWKEY",
		oldRelease: "", newRelease: goldenReleaseBody(),
	}
	c, node := newFlashCluster(t, d)
	meta := &GoldenMeta{BuildID: goldenID}

	res := c.flashOne(context.Background(), flashTestServer(t), GoldenImage, meta, node, FlashOptions{})

	if !res.OK() {
		t.Fatalf("flashOne: %v", res.Err)
	}
	if res.Skipped {
		t.Error("a node with no release marker was skipped")
	}
	if !d.seen("push:" + FlagReflash) {
		t.Error("no reflash flag was armed")
	}
}

// TestFlashForceOverridesTheSkip covers -force: the node is golden and comes
// back golden, and it is rewritten anyway.
func TestFlashForceOverridesTheSkip(t *testing.T) {
	sandboxHome(t)
	d := &flashDialer{
		downFrom: 2, upFrom: 4,
		oldKey: "ssh-ed25519 OLDKEY", newKey: "ssh-ed25519 NEWKEY",
		oldRelease: goldenReleaseBody(), newRelease: goldenReleaseBody(),
	}
	c, node := newFlashCluster(t, d)
	meta := &GoldenMeta{BuildID: goldenID}

	res := c.flashOne(context.Background(), flashTestServer(t), GoldenImage, meta, node, FlashOptions{Force: true})

	if !res.OK() {
		t.Fatalf("flashOne: %v", res.Err)
	}
	if res.Skipped {
		t.Error("-force did not override the skip")
	}
	if !d.seen("push:" + FlagReflash) {
		t.Error("no reflash flag was armed")
	}
}

// TestFlashGuardFailsOpenWithoutMeta: with no golden meta there is nothing to
// compare against, so the guard is inert and the flash runs.
func TestFlashGuardFailsOpenWithoutMeta(t *testing.T) {
	sandboxHome(t)
	d := &flashDialer{
		downFrom: 2, upFrom: 4,
		oldKey: "ssh-ed25519 OLDKEY", newKey: "ssh-ed25519 NEWKEY",
		oldRelease: goldenReleaseBody(), newRelease: goldenReleaseBody(),
	}
	c, node := newFlashCluster(t, d)

	res := c.flashOne(context.Background(), flashTestServer(t), GoldenImage, nil /* meta */, node, FlashOptions{})

	if !res.OK() {
		t.Fatalf("flashOne: %v", res.Err)
	}
	if res.Skipped {
		t.Error("the guard skipped a node with no golden meta to compare against")
	}
	if !d.seen("push:" + FlagReflash) {
		t.Error("no reflash flag was armed")
	}
}

// TestBakeReflashIgnoresTheSkipGuard: bake reflashes the builder with the
// *vanilla* image to start from a clean install, so it must rewrite the card
// even when the builder is already running the golden build. bakeReflash does
// not go through flashOne; this test keeps it that way.
func TestBakeReflashIgnoresTheSkipGuard(t *testing.T) {
	sandboxHome(t)
	d := &flashDialer{
		downFrom: 2, upFrom: 4,
		oldKey: "ssh-ed25519 OLDKEY", newKey: "ssh-ed25519 NEWKEY",
		oldRelease: goldenReleaseBody(), newRelease: goldenReleaseBody(),
	}
	c, node := newFlashCluster(t, d)

	if err := c.bakeReflash(context.Background(), flashTestServer(t), node); err != nil {
		t.Fatalf("bakeReflash: %v", err)
	}
	if !d.seen("push:" + FlagReflash) {
		t.Error("bake's builder reflash was skipped — it must never consult the flash guard")
	}
	if !d.seen("reboot") {
		t.Error("bake's builder reflash never rebooted the node")
	}
}
