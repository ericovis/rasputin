//go:build linux

package agent

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/vishvananda/netlink"

	"github.com/ericovis/rasputin/internal/kmsg"
)

// Iface is the only interface the agent ever configures. A Pi 3's ethernet
// is a fixed on-board USB device (smsc95xx), so the name never varies and
// enumerating interfaces would only add ways to pick the wrong one.
const Iface = "eth0"

const (
	dhcpRetryInterval  = 3 * time.Second
	dhcpAttemptTimeout = 20 * time.Second
	carrierWait        = 30 * time.Second
	linkPollTick       = time.Second
)

// NetworkUp brings up lo and eth0 and configures eth0 from DHCP, retrying
// forever. It only returns once the interface has an address: without the
// network there is nothing the recovery agent can usefully do, and giving up
// would strand a node that is merely waiting for a switch port to come up.
func NetworkUp(ctx context.Context, log *kmsg.Logger) (net.IP, error) {
	if err := linkUp("lo"); err != nil {
		log.Printf("WARNING: bringing up lo: %v", err)
	}
	link, err := waitForLink(ctx, log)
	if err != nil {
		return nil, err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("link set %s up: %w", Iface, err)
	}
	waitForCarrier(ctx, log)

	for attempt := 1; ; attempt++ {
		ip, err := dhcpConfigure(ctx, log)
		if err == nil {
			log.Printf("%s up: %s", Iface, ip)
			return ip, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Printf("DHCP attempt %d failed on %s: %v — retrying in %s", attempt, Iface, err, dhcpRetryInterval)
		sleepCtx(ctx, dhcpRetryInterval)
	}
}

func linkUp(name string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	return netlink.LinkSetUp(l)
}

// waitForLink blocks until the kernel has probed the ethernet driver. If it
// never does, the driver is missing from the kernel and the node needs a
// human, so say so loudly rather than waiting silently.
func waitForLink(ctx context.Context, log *kmsg.Logger) (netlink.Link, error) {
	for waited := time.Duration(0); ; waited += linkPollTick {
		link, err := netlink.LinkByName(Iface)
		if err == nil {
			return link, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if waited > 0 && waited%(10*time.Second) == 0 {
			log.Printf("still waiting for %s after %s — driver missing from the kernel?", Iface, waited)
		}
		sleepCtx(ctx, linkPollTick)
	}
}

// waitForCarrier gives the switch time to bring the port up. It is advisory:
// DHCP is retried forever anyway, so a missing carrier only costs a few
// wasted attempts.
func waitForCarrier(ctx context.Context, log *kmsg.Logger) {
	deadline := time.Now().Add(carrierWait)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		state, err := os.ReadFile("/sys/class/net/" + Iface + "/operstate")
		if err == nil && strings.TrimSpace(string(state)) == "up" {
			return
		}
		sleepCtx(ctx, linkPollTick)
	}
	log.Printf("%s has no carrier after %s; trying DHCP anyway", Iface, carrierWait)
}

// dhcpConfigure runs one DORA exchange and applies the result: address,
// then default route. No renewal loop — a recovery session is minutes long,
// far inside any sane lease, and a renewal goroutine is one more thing that
// can wedge PID 1. A later network error re-runs this instead.
func dhcpConfigure(ctx context.Context, log *kmsg.Logger) (net.IP, error) {
	client, err := nclient4.New(Iface)
	if err != nil {
		return nil, fmt.Errorf("dhcp client on %s: %w", Iface, err)
	}
	defer client.Close()

	reqCtx, cancel := context.WithTimeout(ctx, dhcpAttemptTimeout)
	defer cancel()
	lease, err := client.Request(reqCtx)
	if err != nil {
		return nil, err
	}
	ack := lease.ACK
	ip := ack.YourIPAddr
	if ip == nil || ip.IsUnspecified() {
		return nil, fmt.Errorf("DHCP ACK carried no address")
	}
	mask := ack.SubnetMask()
	if mask == nil {
		mask = ip.DefaultMask()
	}

	link, err := netlink.LinkByName(Iface)
	if err != nil {
		return nil, err
	}
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: mask}}
	if err := netlink.AddrReplace(link, addr); err != nil {
		return nil, fmt.Errorf("assigning %s to %s: %w", addr, Iface, err)
	}
	if routers := ack.Router(); len(routers) > 0 {
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Gw: routers[0]}
		if err := netlink.RouteReplace(route); err != nil {
			// A default route is not needed when the CLI is on the same
			// subnet, which is the normal case, so this is not fatal.
			log.Printf("WARNING: setting default route via %s: %v", routers[0], err)
		}
	}
	log.Printf("DHCP lease: ip=%s mask=%s gw=%v lease=%s",
		ip, net.IP(mask), ack.Router(), ack.IPAddressLeaseTime(0))
	return ip, nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
