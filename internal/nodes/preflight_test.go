package nodes

import (
	"fmt"
	"strings"
	"testing"
)

const recoverySize = 2_700_000

func healthyReplies() map[string]string {
	return map[string]string{
		"uname -m": "aarch64\n",
		"mountpoint -q " + BootMount + " && echo mounted": "mounted\n",
		"sudo -n true": "",
		"df -k --output=avail " + BootMount + " | tail -1": "   480000\n",
	}
}

func TestPreflightPasses(t *testing.T) {
	conn := &fakeConn{host: "rasputin001.local", user: "berry", replies: healthyReplies()}
	p := RunPreflight(conn, "rasputin001", recoverySize)
	if !p.OK() {
		t.Errorf("preflight failed on a healthy node:\n%s", p)
	}
	if len(p.Checks) != 4 {
		t.Errorf("ran %d checks, want 4", len(p.Checks))
	}
	if p.User != "berry" || p.Host != "rasputin001.local" {
		t.Errorf("preflight = %+v", p)
	}
	if len(p.Failures()) != 0 {
		t.Errorf("Failures = %+v", p.Failures())
	}
}

func TestPreflightCatchesWrongArchitecture(t *testing.T) {
	r := healthyReplies()
	r["uname -m"] = "armv7l\n"
	p := RunPreflight(&fakeConn{user: "berry", replies: r}, "n", recoverySize)
	if p.OK() {
		t.Error("preflight passed a 32-bit node")
	}
	if got := p.Failures()[0].Name; got != "architecture" {
		t.Errorf("failure = %q, want architecture", got)
	}
}

func TestPreflightCatchesMissingSudo(t *testing.T) {
	r := healthyReplies()
	delete(r, "sudo -n true")
	conn := &fakeConn{user: "ericovis", replies: r,
		errs: map[string]error{"sudo -n true": fmt.Errorf("a password is required")}}
	p := RunPreflight(conn, "rasputin002", recoverySize)
	if p.OK() {
		t.Error("preflight passed a node without passwordless sudo")
	}
	f := p.Failures()[0]
	if f.Name != "passwordless sudo" {
		t.Fatalf("failure = %+v", f)
	}
	// The message must tell the operator exactly how to fix it: this is the
	// blocker standing in the way of two nodes right now.
	if !strings.Contains(f.Detail, "NOPASSWD:ALL") || !strings.Contains(f.Detail, "ericovis") {
		t.Errorf("detail = %q, want the remedy naming the user", f.Detail)
	}
}

func TestPreflightCatchesAFullBootPartition(t *testing.T) {
	r := healthyReplies()
	r["df -k --output=avail "+BootMount+" | tail -1"] = "  100\n" // 100 KiB
	p := RunPreflight(&fakeConn{user: "berry", replies: r}, "n", recoverySize)
	if p.OK() {
		t.Error("preflight passed a node with no room for recovery.gz")
	}
	if got := p.Failures()[0].Name; got != "boot partition space" {
		t.Errorf("failure = %q", got)
	}
}

func TestPreflightCatchesAnUnmountedBootPartition(t *testing.T) {
	r := healthyReplies()
	delete(r, "mountpoint -q "+BootMount+" && echo mounted")
	conn := &fakeConn{user: "berry", replies: r,
		errs: map[string]error{"mountpoint -q " + BootMount + " && echo mounted": fmt.Errorf("exited 1")}}
	p := RunPreflight(conn, "n", recoverySize)
	if p.OK() {
		t.Error("preflight passed a node with no boot partition mounted")
	}
}

func TestPreflightStringListsEveryCheck(t *testing.T) {
	p := RunPreflight(&fakeConn{user: "berry", replies: healthyReplies()}, "n", recoverySize)
	s := p.String()
	for _, want := range []string{"architecture", "boot partition", "passwordless sudo", "ok"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() is missing %q:\n%s", want, s)
		}
	}
}

func TestParseKB(t *testing.T) {
	got, err := parseKB("  480000\n")
	if err != nil || got != 480000*1024 {
		t.Errorf("parseKB = %d, %v", got, err)
	}
	if _, err := parseKB(""); err == nil {
		t.Error("parseKB accepted empty output")
	}
	if _, err := parseKB("Avail"); err == nil {
		t.Error("parseKB accepted a header line")
	}
}
