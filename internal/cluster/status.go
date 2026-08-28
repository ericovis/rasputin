package cluster

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/nodes"
)

// StatusTimeout bounds each node's whole probe, and StatusDialTimeout bounds
// each address tried within it. Status must stay fast when the cluster is
// half down, but a node whose mDNS name has gone stale still has to get a
// fair try at its ARP address — that is exactly how a misnamed node is
// found.
const (
	StatusTimeout     = 25 * time.Second
	StatusDialTimeout = 6 * time.Second
)

// ReleaseFile is written by firstrun.sh on every provisioned node.
const ReleaseFile = "/etc/rasputin-release"

// ProvisionedMarker is touched once the package set is installed.
const ProvisionedMarker = "/var/lib/rasputin/provisioned"

// Status is one node's answers.
type Status struct {
	Name        string
	MAC         string
	IP          string
	Reachable   bool
	SSHUser     string
	Hostname    string
	BuildID     string
	Uptime      string
	Provisioned bool
	Adopted     bool
	Err         error
}

// Status probes every node in parallel and never modifies anything.
func (c *Cluster) Status(ctx context.Context, targets []config.Node) []Status {
	out := make([]Status, len(targets))
	var wg sync.WaitGroup
	for i, node := range targets {
		wg.Add(1)
		go func(i int, node config.Node) {
			defer wg.Done()
			out[i] = c.statusOf(ctx, node)
		}(i, node)
	}
	wg.Wait()
	return out
}

func (c *Cluster) statusOf(ctx context.Context, node config.Node) Status {
	s := Status{Name: node.Name, MAC: node.MAC}
	if cached, ok := c.State.Get(node.Name); ok {
		s.IP = cached.IP
	}

	ctx, cancel := context.WithTimeout(ctx, StatusTimeout)
	defer cancel()
	res := *c.Resolver
	res.DialTimeout = StatusDialTimeout
	conn, err := res.Connect(ctx, node)
	if err != nil {
		s.Err = err
		return s
	}
	defer conn.Close()

	s.Reachable = true
	s.SSHUser = conn.User()
	s.IP = conn.Host()

	s.Hostname, _ = conn.Output("hostname")
	s.Uptime, _ = conn.Output("uptime -p 2>/dev/null || uptime")
	s.BuildID = buildIDFrom(mustOutput(conn, "cat "+ReleaseFile+" 2>/dev/null || true"))
	s.Provisioned = strings.TrimSpace(mustOutput(conn,
		"test -f "+ProvisionedMarker+" && echo yes || echo no")) == "yes"
	s.Adopted = strings.Contains(
		mustOutput(conn, "cat "+ConfigPath+" 2>/dev/null || true"), "initramfs recovery.gz")
	return s
}

func mustOutput(conn nodes.Conn, cmd string) string {
	out, err := conn.Output(cmd)
	if err != nil {
		return ""
	}
	return out
}

// buildIDFrom extracts build_id= from an /etc/rasputin-release body.
func buildIDFrom(release string) string {
	for _, line := range strings.Split(release, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "build_id="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// StatusTable renders statuses as an aligned table.
func StatusTable(rows []Status) string {
	header := []string{"NODE", "MAC", "ADDRESS", "SSH", "HOSTNAME", "BUILD", "PROV", "UPTIME"}
	cells := [][]string{header}
	for _, r := range rows {
		cells = append(cells, []string{
			r.Name,
			r.MAC,
			dash(r.IP),
			sshCell(r),
			dash(r.Hostname),
			buildCell(r),
			boolCell(r.Reachable, r.Provisioned),
			dash(shorten(r.Uptime, 28)),
		})
	}

	widths := make([]int, len(header))
	for _, row := range cells {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	for _, row := range cells {
		for i, cell := range row {
			if i == len(row)-1 {
				b.WriteString(cell)
			} else {
				fmt.Fprintf(&b, "%-*s  ", widths[i], cell)
			}
		}
		b.WriteByte('\n')
	}
	// Anything that failed gets its reason spelled out under the table:
	// "unreachable" alone is never enough to act on.
	var problems []string
	for _, r := range rows {
		if r.Err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", r.Name, r.Err))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		b.WriteString("\n")
		for _, p := range problems {
			b.WriteString(p + "\n")
		}
	}
	return b.String()
}

func sshCell(r Status) string {
	if !r.Reachable {
		return "down"
	}
	return r.SSHUser
}

func buildCell(r Status) string {
	switch {
	case !r.Reachable:
		return "-"
	case r.BuildID != "":
		return r.BuildID
	case r.Adopted:
		return "stock (adopted)"
	default:
		return "stock"
	}
}

func boolCell(reachable, v bool) string {
	if !reachable {
		return "-"
	}
	if v {
		return "yes"
	}
	return "no"
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return strings.TrimSpace(s)
}

func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
