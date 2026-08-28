package bootfs

import (
	"os"
	"strings"
	"testing"
)

func stockConfig(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/stock-config.txt")
	if err != nil {
		t.Fatalf("reading the stock config.txt fixture: %v", err)
	}
	return data
}

func stockCmdline(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/stock-cmdline.txt")
	if err != nil {
		t.Fatalf("reading the stock cmdline.txt fixture: %v", err)
	}
	return data
}

func TestPatchConfigTxtOnTheStockImage(t *testing.T) {
	got := string(PatchConfigTxt(stockConfig(t)))

	if strings.Contains(got, "auto_initramfs=") {
		t.Error("auto_initramfs is still set; it would load the distribution initramfs instead of ours")
	}
	for _, want := range []string{InitramfsLine, "dtoverlay=disable-wifi", "dtoverlay=disable-bt", configMarkerStart, configMarkerEnd} {
		if !strings.Contains(got, want) {
			t.Errorf("patched config.txt is missing %q", want)
		}
	}
	// Stock settings must survive.
	for _, want := range []string{"arm_64bit=1", "dtparam=audio=on", "dtoverlay=vc4-kms-v3d"} {
		if !strings.Contains(got, want) {
			t.Errorf("patched config.txt lost the stock setting %q", want)
		}
	}
	// Our block must be last, and inside an [all] section.
	block := got[strings.Index(got, configMarkerStart):]
	if !strings.Contains(block, "[all]") {
		t.Error("the managed block is not scoped to [all]")
	}
	if strings.Index(block, "[all]") > strings.Index(block, InitramfsLine) {
		t.Error("[all] must come before the initramfs line")
	}
}

func TestPatchConfigTxtIsIdempotent(t *testing.T) {
	once := PatchConfigTxt(stockConfig(t))
	twice := PatchConfigTxt(once)
	if string(once) != string(twice) {
		t.Errorf("patching twice changed the file:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	if n := strings.Count(string(twice), InitramfsLine); n != 1 {
		t.Errorf("the initramfs line appears %d times, want 1", n)
	}
}

func TestPatchConfigTxtOnAnEmptyFile(t *testing.T) {
	got := string(PatchConfigTxt(nil))
	if !strings.Contains(got, InitramfsLine) {
		t.Errorf("patched empty config.txt = %q", got)
	}
}

func TestWithFirstrunOnTheStockCmdline(t *testing.T) {
	got := strings.TrimSpace(string(WithFirstrun(stockCmdline(t))))
	tokens := strings.Fields(got)

	if strings.Count(got, "\n") != 0 {
		t.Error("cmdline.txt must stay a single line")
	}
	for _, want := range []string{
		"root=PARTUUID=041bba91-02", "rootfstype=ext4", "fsck.repair=yes", "rootwait",
		"cgroup_enable=memory", "cgroup_memory=1",
		"systemd.run=" + FirstrunPath, "systemd.run_success_action=reboot",
		"systemd.unit=kernel-command-line.target",
	} {
		if !contains(tokens, want) {
			t.Errorf("patched cmdline.txt is missing %q\ngot: %s", want, got)
		}
	}
	if contains(tokens, "resize") {
		t.Error("the stock `resize` auto-expansion token is still present; captures would be 32 GB")
	}
}

func TestWithFirstrunRemovesOlderInitHooks(t *testing.T) {
	old := []byte("console=tty1 root=PARTUUID=aa-02 rootwait init=/usr/lib/raspberrypi-sys-mods/init_resize.sh\n")
	got := string(WithFirstrun(old))
	if strings.Contains(got, "init_resize") {
		t.Errorf("the init= auto-expansion hook survived: %s", got)
	}
	if !strings.Contains(got, "root=PARTUUID=aa-02") {
		t.Errorf("the root= token was lost: %s", got)
	}
}

func TestFirstrunRoundTrip(t *testing.T) {
	stock := stockCmdline(t)
	with := WithFirstrun(stock)
	without := WithoutFirstrun(with)

	if strings.Contains(string(without), "systemd.run") {
		t.Errorf("WithoutFirstrun left the triplet behind: %s", without)
	}
	// Removing the triplet must not undo the rest of the patch: this is the
	// file firstrun.sh writes back, and the node keeps booting from it.
	for _, want := range []string{"cgroup_enable=memory", "cgroup_memory=1", "root=PARTUUID=041bba91-02"} {
		if !strings.Contains(string(without), want) {
			t.Errorf("WithoutFirstrun lost %q: %s", want, without)
		}
	}
	if string(WithFirstrun(with)) != string(with) {
		t.Error("WithFirstrun is not idempotent")
	}
	if string(WithoutFirstrun(without)) != string(without) {
		t.Error("WithoutFirstrun is not idempotent")
	}
	if !strings.HasSuffix(string(with), "\n") {
		t.Error("cmdline.txt must end with a newline")
	}
}

func TestPatchCmdlineKeepsOnlyTheFirstLine(t *testing.T) {
	got := string(WithoutFirstrun([]byte("\n\nconsole=tty1 rootwait\ngarbage second line\n")))
	if strings.Contains(got, "garbage") {
		t.Errorf("a stray second line survived: %q", got)
	}
}

func TestNodesConf(t *testing.T) {
	got := string(NodesConf([][2]string{
		{"B8:27:EB:01:02:03", "rasputin001"},
		{"b8:27:eb:04:05:06", "rasputin002"},
	}))
	if !strings.Contains(got, "b8:27:eb:01:02:03\trasputin001\n") {
		t.Errorf("nodes.conf = %q, want lowercase mac<TAB>name lines", got)
	}
	if !strings.Contains(got, "b8:27:eb:04:05:06\trasputin002\n") {
		t.Errorf("nodes.conf = %q", got)
	}
	if !strings.HasPrefix(got, "#") {
		t.Error("nodes.conf should start with a comment explaining the format")
	}
}
