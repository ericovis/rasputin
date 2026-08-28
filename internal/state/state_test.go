package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissingFileIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.All()) != 0 {
		t.Errorf("a missing cache loaded %d nodes", len(s.All()))
	}
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Seen("rasputin001", "192.168.0.74", "berry"); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if err := s.SetHostKey("rasputin001", "ssh-ed25519 AAAA..."); err != nil {
		t.Fatal(err)
	}
	if err := s.Update("rasputin001", func(n *Node) { n.BuildID = "20260828T1-abc" }); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	n, ok := reloaded.Get("rasputin001")
	if !ok {
		t.Fatal("the node was not persisted")
	}
	if n.IP != "192.168.0.74" || n.SSHUser != "berry" || n.BuildID != "20260828T1-abc" {
		t.Errorf("node = %+v", n)
	}
	if reloaded.HostKey("rasputin001") != "ssh-ed25519 AAAA..." {
		t.Errorf("host key = %q", reloaded.HostKey("rasputin001"))
	}
	if n.LastSeen.IsZero() || time.Since(n.LastSeen) > time.Minute {
		t.Errorf("last seen = %v", n.LastSeen)
	}
	if n.Name != "rasputin001" {
		t.Errorf("name = %q", n.Name)
	}
}

func TestForgetHostKey(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "state.json"))
	s.SetHostKey("n", "key")
	if err := s.ForgetHostKey("n"); err != nil {
		t.Fatal(err)
	}
	if got := s.HostKey("n"); got != "" {
		t.Errorf("host key = %q after ForgetHostKey", got)
	}
	// Everything else about the node survives.
	s.Seen("n2", "1.2.3.4", "berry")
	s.SetHostKey("n2", "key2")
	s.ForgetHostKey("n2")
	if n, _ := s.Get("n2"); n.IP != "1.2.3.4" {
		t.Errorf("ForgetHostKey lost the address: %+v", n)
	}
}

func TestCorruptFileIsTreatedAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("a corrupt cache should not be fatal: %v", err)
	}
	if len(s.All()) != 0 {
		t.Error("a corrupt cache produced nodes")
	}
	// And it must be writable again.
	if err := s.Seen("n", "1.2.3.4", "berry"); err != nil {
		t.Fatalf("Seen after a corrupt load: %v", err)
	}
}

func TestGetUnknownNode(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "state.json"))
	if _, ok := s.Get("nope"); ok {
		t.Error("Get invented a node")
	}
	if s.HostKey("nope") != "" {
		t.Error("HostKey invented a key")
	}
}

func TestAllReturnsACopy(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "state.json"))
	s.Seen("n", "1.2.3.4", "berry")
	all := s.All()
	all["n"] = Node{IP: "changed"}
	if n, _ := s.Get("n"); n.IP != "1.2.3.4" {
		t.Error("All returned the live map")
	}
}

func TestSaveIsAtomicAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	s, _ := Load(path)
	if err := s.Seen("n", "1.2.3.4", "berry"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the cache was not created: %v", err)
	}
	// It records which accounts exist on the cluster; keep it to the owner.
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("the staging file was left behind")
	}
}
