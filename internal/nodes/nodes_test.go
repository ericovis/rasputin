package nodes

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/sshx"
	"github.com/ericovis/rasputin/internal/state"
)

const testYAML = `
cluster: rasputin
image:
  source_url: https://example.invalid/x.img.xz
ssh:
  key: /dev/null
  users: [berry, ericovis]
provision:
  user: berry
  authorized_keys: /dev/null
builder: rasputin001
nodes:
  - { name: rasputin001, mac: "b8:27:eb:01:02:03" }
  - { name: rasputin002, mac: "b8:27:eb:04:05:06" }
  - { name: rasputin003, mac: "b8:27:eb:07:08:09" }
`

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(testYAML), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// fakeConn is a scripted SSH connection.
type fakeConn struct {
	host    string
	user    string
	replies map[string]string
	errs    map[string]error
	ran     []string
	closed  bool
}

func (c *fakeConn) Run(cmd string) (sshx.Result, error) {
	c.ran = append(c.ran, cmd)
	if err, ok := c.errs[cmd]; ok {
		return sshx.Result{ExitCode: 1}, err
	}
	out, ok := c.replies[cmd]
	if !ok {
		return sshx.Result{ExitCode: 127}, fmt.Errorf("unexpected command %q", cmd)
	}
	return sshx.Result{Stdout: out}, nil
}

func (c *fakeConn) Sudo(cmd string) (sshx.Result, error) { return c.Run("sudo -n " + cmd) }
func (c *fakeConn) Output(cmd string) (string, error) {
	res, err := c.Run(cmd)
	return strings.TrimSpace(res.Stdout), err
}
func (c *fakeConn) Push(string, []byte, string) error { return nil }
func (c *fakeConn) Fetch(string) ([]byte, error)      { return nil, nil }
func (c *fakeConn) User() string                      { return c.user }
func (c *fakeConn) Host() string                      { return c.host }
func (c *fakeConn) Close() error                      { c.closed = true; return nil }

// fakeDialer answers for a fixed set of hosts.
type fakeDialer struct {
	hosts   map[string]*fakeConn
	dialed  []string
	failAll error
}

func (d *fakeDialer) Dial(_ context.Context, node, host string) (Conn, error) {
	d.dialed = append(d.dialed, host)
	if d.failAll != nil {
		return nil, d.failAll
	}
	c, ok := d.hosts[host]
	if !ok {
		return nil, fmt.Errorf("no route to %s", host)
	}
	return c, nil
}

func macReply(mac string) map[string]string {
	return map[string]string{"cat " + MACPath: mac + "\n"}
}

func newResolver(t *testing.T, d *fakeDialer) *Resolver {
	t.Helper()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Resolver{
		Cfg:   testCfg(t),
		State: st,
		Dial:  d,
		ARP:   func() (map[string]string, error) { return nil, nil },
	}
}

func TestSelect(t *testing.T) {
	r := newResolver(t, &fakeDialer{})
	r.State.Update("rasputin002", func(n *state.Node) { n.IP = "192.168.0.124" })

	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"all"}, []string{"rasputin001", "rasputin002", "rasputin003"}},
		{[]string{"ALL"}, []string{"rasputin001", "rasputin002", "rasputin003"}},
		{[]string{"rasputin002"}, []string{"rasputin002"}},
		{[]string{"b8:27:eb:01:02:03"}, []string{"rasputin001"}},
		{[]string{"B8:27:EB:01:02:03"}, []string{"rasputin001"}},
		{[]string{"192.168.0.124"}, []string{"rasputin002"}},
		// Order follows the config, and duplicates collapse.
		{[]string{"rasputin003", "rasputin001", "rasputin001"}, []string{"rasputin001", "rasputin003"}},
	}
	for _, tc := range cases {
		got, err := r.Select(tc.args)
		if err != nil {
			t.Fatalf("Select(%v): %v", tc.args, err)
		}
		var names []string
		for _, n := range got {
			names = append(names, n.Name)
		}
		if strings.Join(names, ",") != strings.Join(tc.want, ",") {
			t.Errorf("Select(%v) = %v, want %v", tc.args, names, tc.want)
		}
	}
}

func TestSelectRejectsUnknownArguments(t *testing.T) {
	r := newResolver(t, &fakeDialer{})
	if _, err := r.Select(nil); err == nil {
		t.Error("Select accepted no arguments")
	}
	if _, err := r.Select([]string{"rasputin099"}); err == nil {
		t.Error("Select accepted an unknown node")
	}
	if _, err := r.Select([]string{"10.0.0.1"}); err == nil {
		t.Error("Select accepted an IP no node was seen at")
	}
}

func TestCandidateOrder(t *testing.T) {
	r := newResolver(t, &fakeDialer{})
	r.ARP = func() (map[string]string, error) {
		return map[string]string{"b8:27:eb:01:02:03": "192.168.0.74"}, nil
	}
	r.State.Update("rasputin001", func(n *state.Node) { n.IP = "192.168.0.50" })

	got := r.Candidates(*r.Cfg.Node("rasputin001"))
	want := []Candidate{
		{"192.168.0.50", "cache"},
		{"rasputin001.local", "mDNS"},
		{"192.168.0.74", "arp"},
	}
	if len(got) != len(want) {
		t.Fatalf("Candidates = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCandidatesDeduplicate(t *testing.T) {
	r := newResolver(t, &fakeDialer{})
	r.ARP = func() (map[string]string, error) {
		return map[string]string{"b8:27:eb:01:02:03": "192.168.0.74"}, nil
	}
	r.State.Update("rasputin001", func(n *state.Node) { n.IP = "192.168.0.74" })
	got := r.Candidates(*r.Cfg.Node("rasputin001"))
	if len(got) != 2 {
		t.Errorf("Candidates = %+v, want the duplicate address collapsed", got)
	}
}

func TestConnectVerifiesTheMAC(t *testing.T) {
	// The cached address answers, but it is a different Pi. mDNS then
	// reaches the right one.
	imposter := &fakeConn{host: "192.168.0.50", user: "ericovis", replies: macReply("b8:27:eb:04:05:06")}
	real := &fakeConn{host: "rasputin001.local", user: "berry", replies: macReply("b8:27:eb:01:02:03")}
	d := &fakeDialer{hosts: map[string]*fakeConn{
		"192.168.0.50":      imposter,
		"rasputin001.local": real,
	}}
	r := newResolver(t, d)
	r.State.Update("rasputin001", func(n *state.Node) { n.IP = "192.168.0.50" })

	conn, err := r.Connect(context.Background(), *r.Cfg.Node("rasputin001"))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.Host() != "rasputin001.local" {
		t.Errorf("connected to %s, want the node whose MAC matched", conn.Host())
	}
	if !imposter.closed {
		t.Error("the mismatched connection was left open")
	}

	// The successful address is remembered for next time.
	s, _ := r.State.Get("rasputin001")
	if s.IP != "rasputin001.local" || s.SSHUser != "berry" {
		t.Errorf("state = %+v, want the successful address and user recorded", s)
	}
}

func TestConnectFailsWhenEveryCandidateIsWrong(t *testing.T) {
	wrong := &fakeConn{host: "rasputin001.local", user: "ericovis", replies: macReply("aa:bb:cc:dd:ee:ff")}
	d := &fakeDialer{hosts: map[string]*fakeConn{"rasputin001.local": wrong}}
	r := newResolver(t, d)

	_, err := r.Connect(context.Background(), *r.Cfg.Node("rasputin001"))
	if err == nil {
		t.Fatal("Connect returned a machine with the wrong MAC")
	}
	if !strings.Contains(err.Error(), "refusing to touch it") {
		t.Errorf("err = %v, want it to explain the MAC mismatch", err)
	}
}

func TestConnectReportsEveryAttempt(t *testing.T) {
	r := newResolver(t, &fakeDialer{failAll: fmt.Errorf("connection refused")})
	_, err := r.Connect(context.Background(), *r.Cfg.Node("rasputin003"))
	if err == nil {
		t.Fatal("Connect succeeded with no reachable address")
	}
	if !strings.Contains(err.Error(), "rasputin003.local") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v, want each attempt listed", err)
	}
}

func TestReachable(t *testing.T) {
	good := &fakeConn{host: "rasputin001.local", user: "berry", replies: macReply("b8:27:eb:01:02:03")}
	r := newResolver(t, &fakeDialer{hosts: map[string]*fakeConn{"rasputin001.local": good}})
	if !r.Reachable(context.Background(), *r.Cfg.Node("rasputin001")) {
		t.Error("Reachable said no for a node that answers")
	}
	if !good.closed {
		t.Error("Reachable leaked the connection")
	}
	if r.Reachable(context.Background(), *r.Cfg.Node("rasputin002")) {
		t.Error("Reachable said yes for an unreachable node")
	}
}

func TestWaitForTimesOut(t *testing.T) {
	r := newResolver(t, &fakeDialer{failAll: fmt.Errorf("down")})
	start := time.Now()
	_, err := r.WaitFor(context.Background(), *r.Cfg.Node("rasputin001"), 50*time.Millisecond)
	if err == nil {
		t.Fatal("WaitFor returned success for a node that never came back")
	}
	if !strings.Contains(err.Error(), "did not come back within") {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("WaitFor overshot its timeout badly")
	}
}

func TestWaitForRespectsContextCancellation(t *testing.T) {
	r := newResolver(t, &fakeDialer{failAll: fmt.Errorf("down")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.WaitFor(ctx, *r.Cfg.Node("rasputin001"), time.Minute); err == nil {
		t.Error("WaitFor ignored a cancelled context")
	}
}

func TestParseARPDarwinFormat(t *testing.T) {
	out := `? (192.168.0.1) at 3c:37:86:1f:2a:9b on en0 ifscope [ethernet]
? (192.168.0.74) at b8:27:eb:01:02:03 on en0 ifscope [ethernet]
? (192.168.0.124) at b8:27:eb:04:05:06 on en0 ifscope [ethernet]
? (192.168.0.255) at ff:ff:ff:ff:ff:ff on en0 ifscope [ethernet]
? (192.168.0.9) at a:1b:2c:3d:4e:5f on en0 ifscope [ethernet]
? (192.168.0.10) at (incomplete) on en0 ifscope [ethernet]`
	table := ParseARP(out)
	if got := table["b8:27:eb:01:02:03"]; got != "192.168.0.74" {
		t.Errorf("lookup = %q, want 192.168.0.74", got)
	}
	if got := table["0a:1b:2c:3d:4e:5f"]; got != "192.168.0.9" {
		t.Errorf("abbreviated MAC not normalised: table = %v", table)
	}
	if _, ok := table["incomplete"]; ok {
		t.Error("an incomplete entry was parsed")
	}
}

func TestNormalizeMAC(t *testing.T) {
	cases := map[string]string{
		"B8:27:EB:01:02:03":     "b8:27:eb:01:02:03",
		"a:1b:2c:3d:4e:5f":      "0a:1b:2c:3d:4e:5f",
		"  b8:27:eb:01:02:03  ": "b8:27:eb:01:02:03",
		"not-a-mac":             "",
		"b8:27:eb:af:ca":        "",
		"b8:27:eb:af:ca:ddd":    "",
	}
	for in, want := range cases {
		if got := NormalizeMAC(in); got != want {
			t.Errorf("NormalizeMAC(%q) = %q, want %q", in, got, want)
		}
	}
}
