package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimal = `
cluster: rasputin
image:
  source_url: https://example.invalid/img.xz
ssh:
  key: ~/.ssh/id_ed25519
  users: [berry, ericovis]
provision:
  user: berry
  authorized_keys: ~/.ssh/id_ed25519.pub
builder: rasputin001
nodes:
  - { name: rasputin001, mac: "B8:27:EB:01:02:03" }
`

func TestParseDefaultsAndNormalization(t *testing.T) {
	c, err := Parse([]byte(minimal), "test.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.Server.ListenPort(); got != DefaultPort {
		t.Errorf("port = %d, want %d", got, DefaultPort)
	}
	if c.Image.RootfsSizeGB != DefaultRootfsSizeGB {
		t.Errorf("rootfs_size_gb = %d, want %d", c.Image.RootfsSizeGB, DefaultRootfsSizeGB)
	}
	if c.Timeouts.FlashMinutes != DefaultFlashMinutes || c.Timeouts.BakeMinutes != DefaultBakeMinutes {
		t.Errorf("timeouts = %+v", c.Timeouts)
	}
	if c.Nodes[0].MAC != "b8:27:eb:01:02:03" {
		t.Errorf("mac = %q, want lowercase colon form", c.Nodes[0].MAC)
	}
	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, ".ssh/id_ed25519"); c.SSH.Key != want {
		t.Errorf("ssh.key = %q, want %q", c.SSH.Key, want)
	}
	if want := filepath.Join(home, ".ssh/id_ed25519.pub"); c.Provision.AuthorizedKeys != want {
		t.Errorf("authorized_keys = %q, want %q", c.Provision.AuthorizedKeys, want)
	}
}

func TestExplicitZeroPortIsKept(t *testing.T) {
	c, err := Parse([]byte(minimal+"server:\n  port: 0\n"), "test.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.Server.ListenPort(); got != 0 {
		t.Errorf("port = %d, want 0 (random free port)", got)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"no nodes", strings.Replace(minimal, `  - { name: rasputin001, mac: "B8:27:EB:01:02:03" }`, "", 1) + "", "at least one node"},
		{"bad mac", strings.Replace(minimal, "B8:27:EB:01:02:03", "not-a-mac", 1), "bad mac"},
		{"unknown builder", strings.Replace(minimal, "builder: rasputin001", "builder: nope", 1), "not a known node"},
		{"empty user", strings.Replace(minimal, "  user: berry", `  user: ""`, 1), "provision.user"},
		{"bad port", minimal + "server:\n  port: 70000\n", "out of range"},
		{"no ssh users", strings.Replace(minimal, "  users: [berry, ericovis]", "  users: []", 1), "ssh.users"},
		{"dup mac", minimal + `  - { name: rasputin002, mac: "b8:27:eb:01:02:03" }` + "\n", "share mac"},
		{"unknown field", minimal + "surprise: 1\n", "field surprise"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml), "test.yaml")
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadRepoConfig(t *testing.T) {
	c, err := Load("../../rasputin.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Nodes) != 4 {
		t.Errorf("nodes = %d, want 4", len(c.Nodes))
	}
	if c.Node(c.Builder) == nil {
		t.Errorf("builder %q not found", c.Builder)
	}
	if got := c.NodeNames(); got[0] != "rasputin001" || got[3] != "rasputin004" {
		t.Errorf("NodeNames = %v", got)
	}
	if len(c.Provision.Packages) != 6 {
		t.Errorf("packages = %v", c.Provision.Packages)
	}
}

func TestSudoModeDefaultsAndValidation(t *testing.T) {
	c, err := Parse([]byte(minimal), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.SSH.Sudo != SudoPasswordless {
		t.Errorf("ssh.sudo = %q, want %q by default", c.SSH.Sudo, SudoPasswordless)
	}
	if c.SSH.NeedsSudoPassword() {
		t.Error("the default mode should not ask for a password")
	}

	withPw := strings.Replace(minimal, "  users: [berry, ericovis]", "  users: [berry, ericovis]\n  sudo: password", 1)
	c, err = Parse([]byte(withPw), "test.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !c.SSH.NeedsSudoPassword() {
		t.Errorf("ssh.sudo = %q should ask for a password", c.SSH.Sudo)
	}

	bad := strings.Replace(minimal, "  users: [berry, ericovis]", "  users: [berry, ericovis]\n  sudo: yolo", 1)
	if _, err := Parse([]byte(bad), "test.yaml"); err == nil {
		t.Error("an unknown ssh.sudo mode was accepted")
	}
}

// TestConfigHasNoPasswordField guards the decision that a sudo password must
// never live in rasputin.yaml, which is committed to git.
func TestConfigHasNoPasswordField(t *testing.T) {
	yamlWithSecret := strings.Replace(minimal,
		"  users: [berry, ericovis]", "  users: [berry, ericovis]\n  sudo_password: hunter2", 1)
	_, err := Parse([]byte(yamlWithSecret), "test.yaml")
	if err == nil {
		t.Fatal("rasputin.yaml accepted a sudo_password field; secrets must not be storable here")
	}
	if !strings.Contains(err.Error(), "sudo_password") {
		t.Errorf("err = %v, want it to name the rejected field", err)
	}
}
