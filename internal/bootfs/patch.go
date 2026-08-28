package bootfs

import (
	"strings"
)

// The managed block appended to config.txt. Everything between the markers is
// ours: rewriting it is how the patch stays idempotent across re-runs.
const (
	configMarkerStart = "# rasputin — managed block, do not edit by hand"
	configMarkerEnd   = "# end rasputin"

	// InitramfsLine loads the recovery agent on every boot. `followkernel`
	// places the initramfs directly after the kernel in memory, which is
	// what the Raspberry Pi firmware expects.
	InitramfsLine = "initramfs recovery.gz followkernel"

	// RecoveryFile is the initramfs name referenced by InitramfsLine.
	RecoveryFile = "recovery.gz"
)

// configLines is the managed block's content, in order.
var configLines = []string{
	// [all] guards against a preceding conditional section (e.g. [pi5])
	// silently scoping our settings to one model.
	"[all]",
	InitramfsLine,
	"dtoverlay=disable-wifi",
	"dtoverlay=disable-bt",
}

// FirstrunPath is where firstrun.sh lives as seen from the booted system.
//
// On Bookworm and later — which includes the Trixie images this tool targets
// — the boot partition is mounted at /boot/firmware, not /boot, and that is
// the path the official Raspberry Pi Imager writes into cmdline.txt.
const FirstrunPath = "/boot/firmware/firstrun.sh"

// firstrunTokens is the triplet that makes systemd run firstrun.sh once, on
// the first boot, and reboot afterwards.
var firstrunTokens = []string{
	"systemd.run=" + FirstrunPath,
	"systemd.run_success_action=reboot",
	"systemd.unit=kernel-command-line.target",
}

// cgroupTokens are needed by podman on Raspberry Pi OS; the kernel does not
// enable the memory cgroup controller by default on arm.
var cgroupTokens = []string{"cgroup_enable=memory", "cgroup_memory=1"}

// PatchConfigTxt removes the stock automatic-initramfs setting and appends
// (or refreshes) the rasputin managed block.
//
// auto_initramfs=1 would load the distribution's own initramfs; the explicit
// `initramfs recovery.gz followkernel` line replaces it, because the recovery
// agent must be what the kernel runs as PID 1.
func PatchConfigTxt(data []byte) []byte {
	body := stripManagedBlock(string(data))

	var kept []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "auto_initramfs="):
			continue
		case strings.HasPrefix(trimmed, "initramfs "):
			// An initramfs line from an earlier tool would fight ours.
			continue
		case trimmed == "dtoverlay=disable-wifi" || trimmed == "dtoverlay=disable-bt":
			continue
		}
		kept = append(kept, line)
	}

	out := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	block := configMarkerStart + "\n" + strings.Join(configLines, "\n") + "\n" + configMarkerEnd
	if out == "" {
		return []byte(block + "\n")
	}
	return []byte(out + "\n\n" + block + "\n")
}

// stripManagedBlock removes a previously appended managed block, so patching
// an already-patched config.txt is a no-op rather than a pile-up.
func stripManagedBlock(s string) string {
	start := strings.Index(s, configMarkerStart)
	if start < 0 {
		return s
	}
	end := strings.Index(s[start:], configMarkerEnd)
	if end < 0 {
		return strings.TrimRight(s[:start], "\n")
	}
	tail := s[start+end+len(configMarkerEnd):]
	return strings.TrimRight(s[:start], "\n") + strings.TrimLeft(tail, "\n")
}

// WithFirstrun returns cmdline.txt patched to run firstrun.sh on the next
// boot: the stock rootfs auto-expansion is removed (firstrun.sh grows the
// root filesystem to a fixed cap instead, so that captures stay small), the
// cgroup tokens are ensured, and the systemd.run triplet is appended.
func WithFirstrun(data []byte) []byte {
	return patchCmdline(data, true)
}

// WithoutFirstrun returns cmdline.txt with the firstrun triplet removed. It
// is what firstrun.sh itself writes back, so that it never runs twice.
func WithoutFirstrun(data []byte) []byte {
	return patchCmdline(data, false)
}

func patchCmdline(data []byte, firstrun bool) []byte {
	// cmdline.txt is a single line; anything else is a mistake we should not
	// propagate, so only the first non-empty line is kept.
	line := ""
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}

	var kept []string
	for _, tok := range strings.Fields(line) {
		if isRemovedToken(tok) {
			continue
		}
		kept = append(kept, tok)
	}
	for _, tok := range cgroupTokens {
		if !contains(kept, tok) {
			kept = append(kept, tok)
		}
	}
	if firstrun {
		kept = append(kept, firstrunTokens...)
	}
	return []byte(strings.Join(kept, " ") + "\n")
}

// isRemovedToken reports whether a cmdline token is one we always strip:
// the stock first-boot hooks, and any firstrun triplet from a previous pass.
func isRemovedToken(tok string) bool {
	switch {
	case strings.HasPrefix(tok, "systemd.run"):
		return true
	case tok == "systemd.unit=kernel-command-line.target":
		return true
	// The stock image expands the root filesystem to the whole card on first
	// boot. Older images do it from an init= hook; the 2026-06 Trixie image
	// uses a bare `resize` token consumed by its own initramfs. Both would
	// turn every capture into a 32 GB stream, so both go.
	case tok == "resize":
		return true
	case strings.HasPrefix(tok, "init=") &&
		(strings.Contains(tok, "firstboot") || strings.Contains(tok, "init_resize")):
		return true
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// NodesConf renders the MAC-to-hostname table the identity service reads on
// every boot, as `mac<TAB>name` lines.
func NodesConf(pairs [][2]string) []byte {
	var b strings.Builder
	b.WriteString("# rasputin node identities: mac<TAB>hostname\n")
	for _, p := range pairs {
		b.WriteString(strings.ToLower(p[0]))
		b.WriteByte('\t')
		b.WriteString(p[1])
		b.WriteByte('\n')
	}
	return []byte(b.String())
}
