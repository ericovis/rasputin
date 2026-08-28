// Package nodes turns what an operator typed on the command line into open
// connections to real machines.
//
// Finding a node is deliberately paranoid. This cluster has had duplicate
// hostnames before, so mDNS alone cannot be trusted: every connection is
// verified against the node's ethernet MAC before anything is done to it.
// Flashing the wrong Pi is the one mistake with no undo.
package nodes

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/sshx"
	"github.com/ericovis/rasputin/internal/state"
)

// AllKeyword selects every node in the config.
const AllKeyword = "all"

// MACPath is where a node reports its own ethernet address.
const MACPath = "/sys/class/net/eth0/address"

// Conn is the subset of an SSH connection this package uses. It is an
// interface so resolution can be tested without hardware.
type Conn interface {
	Run(cmd string) (sshx.Result, error)
	Sudo(cmd string) (sshx.Result, error)
	Output(cmd string) (string, error)
	Push(path string, data []byte, mode string) error
	Fetch(path string) ([]byte, error)
	User() string
	Host() string
	Close() error
}

// Dialer opens connections. sshx.Dialer is adapted to it by Adapt.
type Dialer interface {
	Dial(ctx context.Context, node, host string) (Conn, error)
}

type sshDialer struct{ d *sshx.Dialer }

func (s sshDialer) Dial(ctx context.Context, node, host string) (Conn, error) {
	return s.d.Dial(ctx, node, host)
}

// Adapt wraps a real SSH dialer as a Dialer.
func Adapt(d *sshx.Dialer) Dialer { return sshDialer{d} }

// Resolver finds and connects to nodes.
type Resolver struct {
	Cfg   *config.Config
	State *state.Store
	Dial  Dialer
	// ARP returns a MAC-to-IP map of the local network. Defaults to reading
	// the system ARP table; tests replace it.
	ARP func() (map[string]string, error)
	// Log receives progress lines; nil discards them.
	Log func(format string, args ...any)
	// PollInterval is how often WaitFor retries; defaults to DefaultPoll.
	PollInterval time.Duration
	// DialTimeout bounds each individual candidate address. Without it, one
	// slow candidate (an mDNS name that no longer resolves, say) can eat the
	// whole budget and starve the address that would have worked.
	DialTimeout time.Duration
}

// DefaultDialTimeout bounds one candidate address.
const DefaultDialTimeout = 8 * time.Second

// DefaultPoll is how often WaitFor retries a rebooting node. A Pi 3 takes
// about a minute to boot, so polling faster only adds noise.
const DefaultPoll = 5 * time.Second

func (r *Resolver) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

// Select resolves command-line arguments to configured nodes. An argument
// may be a node name, a MAC, an IP that a node was last seen at, or `all`.
// The result preserves config order and contains no duplicates.
func (r *Resolver) Select(args []string) ([]config.Node, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("no node given (use a node name, a MAC, an IP, or %q)", AllKeyword)
	}
	chosen := map[string]bool{}
	for _, arg := range args {
		if strings.EqualFold(arg, AllKeyword) {
			for _, n := range r.Cfg.Nodes {
				chosen[n.Name] = true
			}
			continue
		}
		n, err := r.match(arg)
		if err != nil {
			return nil, err
		}
		chosen[n.Name] = true
	}
	var out []config.Node
	for _, n := range r.Cfg.Nodes {
		if chosen[n.Name] {
			out = append(out, n)
		}
	}
	return out, nil
}

func (r *Resolver) match(arg string) (config.Node, error) {
	lower := strings.ToLower(arg)
	for _, n := range r.Cfg.Nodes {
		if strings.EqualFold(n.Name, arg) || n.MAC == lower {
			return n, nil
		}
	}
	// An IP only resolves through the cache: the config never holds one.
	if r.State != nil {
		for name, s := range r.State.All() {
			if s.IP != "" && s.IP == arg {
				if n := r.Cfg.Node(name); n != nil {
					return *n, nil
				}
			}
		}
	}
	return config.Node{}, fmt.Errorf("%q is not a known node, MAC or last-seen IP", arg)
}

// Candidate is one address worth trying for a node, and where it came from.
type Candidate struct {
	Host   string
	Source string
}

// Candidates lists the addresses to try for a node, cheapest first: the
// address that worked last time, then mDNS, then the ARP table.
func (r *Resolver) Candidates(node config.Node) []Candidate {
	var out []Candidate
	seen := map[string]bool{}
	add := func(host, source string) {
		if host == "" || seen[host] {
			return
		}
		seen[host] = true
		out = append(out, Candidate{Host: host, Source: source})
	}

	if r.State != nil {
		if s, ok := r.State.Get(node.Name); ok {
			add(s.IP, "cache")
		}
	}
	add(node.Name+".local", "mDNS")
	if arp := r.arpLookup(node.MAC); arp != "" {
		add(arp, "arp")
	}
	return out
}

func (r *Resolver) arpLookup(mac string) string {
	lookup := r.ARP
	if lookup == nil {
		lookup = ARPTable
	}
	table, err := lookup()
	if err != nil {
		r.logf("could not read the ARP table: %v", err)
		return ""
	}
	return table[strings.ToLower(mac)]
}

// Connect opens a verified connection to a node: it tries each candidate
// address in turn and keeps the first one whose eth0 MAC matches the config.
//
// A machine that answers but reports a different MAC is somebody else's Pi.
// It is skipped, loudly.
func (r *Resolver) Connect(ctx context.Context, node config.Node) (Conn, error) {
	candidates := r.Candidates(node)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%s: no address to try", node.Name)
	}
	perDial := r.DialTimeout
	if perDial <= 0 {
		perDial = DefaultDialTimeout
	}
	var problems []string
	for _, c := range candidates {
		dialCtx, cancel := context.WithTimeout(ctx, perDial)
		conn, err := r.Dial.Dial(dialCtx, node.Name, c.Host)
		cancel()
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s (%s): %v", c.Host, c.Source, err))
			continue
		}
		mac, err := conn.Output("cat " + MACPath)
		if err != nil {
			conn.Close()
			problems = append(problems, fmt.Sprintf("%s (%s): cannot read its MAC: %v", c.Host, c.Source, err))
			continue
		}
		mac = strings.ToLower(strings.TrimSpace(mac))
		if mac != node.MAC {
			conn.Close()
			problems = append(problems, fmt.Sprintf(
				"%s (%s): answers as MAC %s, but %s is %s — refusing to touch it",
				c.Host, c.Source, mac, node.Name, node.MAC))
			continue
		}
		if r.State != nil {
			if err := r.State.Seen(node.Name, c.Host, conn.User()); err != nil {
				r.logf("could not update the state cache: %v", err)
			}
		}
		r.logf("%s: connected to %s (%s) as %s", node.Name, c.Host, c.Source, conn.User())
		return conn, nil
	}
	return nil, fmt.Errorf("%s is unreachable:\n  %s", node.Name, strings.Join(problems, "\n  "))
}

// Reachable reports whether a node answers, closing the connection again.
func (r *Resolver) Reachable(ctx context.Context, node config.Node) bool {
	conn, err := r.Connect(ctx, node)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// WaitFor polls until a node answers SSH with the right MAC, or the timeout
// elapses. It is how every reboot in this tool is awaited.
func (r *Resolver) WaitFor(ctx context.Context, node config.Node, timeout time.Duration) (Conn, error) {
	deadline := time.Now().Add(timeout)
	attempt := 0
	for {
		attempt++
		conn, err := r.Connect(ctx, node)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s did not come back within %s (last error: %v)", node.Name, timeout, err)
		}
		if attempt%6 == 0 {
			r.logf("%s: still waiting (%s left)", node.Name, time.Until(deadline).Round(time.Second))
		}
		poll := r.PollInterval
		if poll <= 0 {
			poll = DefaultPoll
		}
		// Never sleep past the deadline: the caller asked for a bound.
		if left := time.Until(deadline); left < poll {
			poll = left
		}
		if poll <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// arpLine matches the darwin `arp -a` format:
//
//	? (192.168.0.74) at b8:27:eb:01:02:03 on en0 ifscope [ethernet]
var arpLine = regexp.MustCompile(`\(([0-9.]+)\) at ([0-9a-fA-F:]+)`)

// ARPTable reads the system ARP table as a MAC-to-IP map. Addresses are
// normalised, since macOS prints them without leading zeros (b8:27:eb:01:02:03
// appears as b8:27:eb:01:02:03 but 0a:... appears as a:...).
func ARPTable() (map[string]string, error) {
	out, err := exec.Command("arp", "-a", "-n").Output()
	if err != nil {
		// Some platforms lack -n; fall back to the plain form.
		out, err = exec.Command("arp", "-a").Output()
		if err != nil {
			return nil, err
		}
	}
	return ParseARP(string(out)), nil
}

// ParseARP extracts a MAC-to-IP map from `arp -a` output.
func ParseARP(out string) map[string]string {
	table := map[string]string{}
	for _, m := range arpLine.FindAllStringSubmatch(out, -1) {
		mac := NormalizeMAC(m[2])
		if mac == "" || table[mac] != "" {
			continue
		}
		table[mac] = m[1]
	}
	return table
}

// NormalizeMAC pads each octet to two digits and lowercases the result, so
// macOS's abbreviated form compares equal to the config's.
func NormalizeMAC(mac string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(mac)), ":")
	if len(parts) != 6 {
		return ""
	}
	for i, p := range parts {
		if len(p) == 0 || len(p) > 2 {
			return ""
		}
		if len(p) == 1 {
			parts[i] = "0" + p
		}
	}
	return strings.Join(parts, ":")
}
