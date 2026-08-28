package nodes

import (
	"fmt"
	"strconv"
	"strings"
)

// BootMount is where Raspberry Pi OS mounts the FAT boot partition on
// Bookworm and later.
const BootMount = "/boot/firmware"

// Check is one preflight condition.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Preflight is everything that must be true before a node can be adopted.
type Preflight struct {
	Node   string
	User   string
	Host   string
	Checks []Check
}

// OK reports whether every check passed.
func (p Preflight) OK() bool {
	for _, c := range p.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// Failures lists the checks that did not pass.
func (p Preflight) Failures() []Check {
	var out []Check
	for _, c := range p.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

func (p Preflight) String() string {
	var b strings.Builder
	for _, c := range p.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  %s %-22s %s\n", mark, c.Name, c.Detail)
	}
	return b.String()
}

// RunPreflight checks that a node can host the recovery mechanism.
// recoverySize is the size of recovery.gz; the boot partition needs room for
// it several times over, because adopt writes a new copy beside the old one.
func RunPreflight(conn Conn, node string, recoverySize int64) Preflight {
	p := Preflight{Node: node, User: conn.User(), Host: conn.Host()}

	arch, err := conn.Output("uname -m")
	p.Checks = append(p.Checks, Check{
		Name:   "architecture",
		OK:     err == nil && arch == "aarch64",
		Detail: detail(err, arch, "want aarch64"),
	})

	_, mountErr := conn.Output("mountpoint -q " + BootMount + " && echo mounted")
	p.Checks = append(p.Checks, Check{
		Name:   "boot partition",
		OK:     mountErr == nil,
		Detail: detail(mountErr, BootMount+" is a mountpoint", ""),
	})

	// -n makes sudo fail rather than wait for a password we cannot supply.
	_, sudoErr := conn.Output("sudo -n true")
	p.Checks = append(p.Checks, Check{
		Name: "passwordless sudo",
		OK:   sudoErr == nil,
		Detail: detail(sudoErr, "sudo -n works",
			"run: echo '"+conn.User()+" ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/"+conn.User()),
	})

	freeOut, freeErr := conn.Output("df -k --output=avail " + BootMount + " | tail -1")
	free, parseErr := parseKB(freeOut)
	want := 2 * recoverySize
	p.Checks = append(p.Checks, Check{
		Name: "boot partition space",
		OK:   freeErr == nil && parseErr == nil && free >= want,
		Detail: func() string {
			if freeErr != nil {
				return freeErr.Error()
			}
			if parseErr != nil {
				return fmt.Sprintf("cannot parse df output %q", freeOut)
			}
			return fmt.Sprintf("%d MiB free, want at least %d MiB", free>>20, want>>20)
		}(),
	})

	return p
}

func detail(err error, ok, hint string) string {
	if err != nil {
		if hint != "" {
			return fmt.Sprintf("%v (%s)", err, hint)
		}
		return err.Error()
	}
	if ok == "" {
		return hint
	}
	return ok
}

// parseKB reads a df kilobyte count.
func parseKB(s string) (int64, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty df output")
	}
	kb, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
	if err != nil {
		return 0, err
	}
	return kb * 1024, nil
}
