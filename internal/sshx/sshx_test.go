package sshx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/state"
)

func dialerFor(t *testing.T, srv *testSSHD, users []string, keys HostKeyStore) *Dialer {
	t.Helper()
	signer, _ := testKey(t)
	return &Dialer{
		Users:   users,
		Auth:    []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Timeout: 3 * time.Second,
		Keys:    keys,
		Port:    srv.Port(),
	}
}

func TestDialTriesUsersInOrder(t *testing.T) {
	// Only the second configured user exists, which is exactly the state of
	// this cluster mid-migration.
	srv := newTestSSHD(t, "ericovis")
	d := dialerFor(t, srv, []string{"berry", "ericovis"}, nil)

	c, err := d.Dial(context.Background(), "rasputin001", "127.0.0.1")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if c.User() != "ericovis" {
		t.Errorf("authenticated as %q, want ericovis", c.User())
	}
	if c.Host() != "127.0.0.1" || c.Node() != "rasputin001" {
		t.Errorf("client = %s@%s (%s)", c.User(), c.Host(), c.Node())
	}
}

func TestDialPrefersTheFirstWorkingUser(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	d := dialerFor(t, srv, []string{"berry", "ericovis"}, nil)
	c, err := d.Dial(context.Background(), "n", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.User() != "berry" {
		t.Errorf("authenticated as %q, want berry", c.User())
	}
}

func TestDialReportsEveryUserItTried(t *testing.T) {
	srv := newTestSSHD(t, "nobody")
	d := dialerFor(t, srv, []string{"berry", "ericovis"}, nil)
	_, err := d.Dial(context.Background(), "n", "127.0.0.1")
	if err == nil {
		t.Fatal("Dial succeeded with no valid user")
	}
	for _, want := range []string{"berry@", "ericovis@"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %s", err, want)
		}
	}
}

func TestDialWithNoUsers(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	d := dialerFor(t, srv, nil, nil)
	if _, err := d.Dial(context.Background(), "n", "127.0.0.1"); err == nil {
		t.Error("Dial accepted an empty user list")
	}
}

func TestHostKeyIsRecordedOnFirstUseAndPinnedAfter(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	st, err := state.Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	d := dialerFor(t, srv, []string{"berry"}, st)

	c, err := d.Dial(context.Background(), "rasputin001", "127.0.0.1")
	if err != nil {
		t.Fatalf("first Dial: %v", err)
	}
	c.Close()
	recorded := st.HostKey("rasputin001")
	if recorded != hostKeyString(srv.HostKey) {
		t.Fatalf("recorded host key = %q, want the server's", recorded)
	}

	// Same key: still fine.
	c2, err := d.Dial(context.Background(), "rasputin001", "127.0.0.1")
	if err != nil {
		t.Fatalf("second Dial: %v", err)
	}
	c2.Close()

	// A different key on the same node name must be refused.
	other := newTestSSHD(t, "berry")
	d2 := dialerFor(t, other, []string{"berry"}, st)
	_, err = d2.Dial(context.Background(), "rasputin001", "127.0.0.1")
	if err == nil {
		t.Fatal("a changed host key was accepted")
	}
	if !strings.Contains(err.Error(), "host key") {
		t.Errorf("err = %v, want it to name the host key", err)
	}

	// Forgetting the key — what a reflash does — makes it work again.
	if err := st.ForgetHostKey("rasputin001"); err != nil {
		t.Fatal(err)
	}
	c3, err := d2.Dial(context.Background(), "rasputin001", "127.0.0.1")
	if err != nil {
		t.Fatalf("Dial after ForgetHostKey: %v", err)
	}
	c3.Close()
	if st.HostKey("rasputin001") != hostKeyString(other.HostKey) {
		t.Error("the new host key was not recorded")
	}
}

func TestHostKeyIsNotRecordedForAnonymousConnections(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	st, _ := state.Load(filepath.Join(t.TempDir(), "state.json"))
	d := dialerFor(t, srv, []string{"berry"}, st)
	c, err := d.Dial(context.Background(), "", "127.0.0.1")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c.Close()
	if len(st.All()) != 0 {
		t.Errorf("an unnamed connection wrote to the cache: %v", st.All())
	}
}

func TestRunCapturesOutput(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	srv.reply("uname -m", "aarch64\n")
	d := dialerFor(t, srv, []string{"berry"}, nil)
	c, err := d.Dial(context.Background(), "n", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	res, err := c.Run("uname -m")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "aarch64\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	out, err := c.Output("uname -m")
	if err != nil || out != "aarch64" {
		t.Errorf("Output = %q, %v", out, err)
	}
}

func TestRunSurfacesTheExitCode(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	srv.fail("sudo -n false", 1)
	d := dialerFor(t, srv, []string{"berry"}, nil)
	c, _ := d.Dial(context.Background(), "n", "127.0.0.1")
	defer c.Close()

	res, err := c.Sudo("false")
	if err == nil {
		t.Fatal("a failing command reported success")
	}
	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.ExitCode)
	}
	if !strings.Contains(err.Error(), "exited 1") {
		t.Errorf("err = %v", err)
	}
	if !containsCmd(srv.ran(), "sudo -n false") {
		t.Errorf("Sudo ran %v, want the command prefixed with sudo -n", srv.ran())
	}
}

// containsCmd reports whether the server saw an exact command.
func containsCmd(ran []string, want string) bool {
	for _, c := range ran {
		if c == want {
			return true
		}
	}
	return false
}

// pushCmdFrom returns the command that carried a file push.
func pushCmdFrom(ran []string) string {
	for _, c := range ran {
		if strings.Contains(c, "cat > ") {
			return c
		}
	}
	return ""
}

func TestPushSendsContentAsRoot(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	d := dialerFor(t, srv, []string{"berry"}, nil)
	c, _ := d.Dial(context.Background(), "n", "127.0.0.1")
	defer c.Close()

	content := []byte("initramfs recovery.gz followkernel\n")
	if err := c.Push("/boot/firmware/config.txt", content, "0644"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	cmd := pushCmdFrom(srv.ran())
	if cmd == "" {
		t.Fatalf("no push command ran; got %v", srv.ran())
	}
	for _, want := range []string{"sudo -n sh -c", "mkdir -p", "/boot/firmware/config.txt", "chmod 0644", "sync"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("push command %q is missing %q", cmd, want)
		}
	}
	if got := srv.stdinFor(cmd); got != string(content) {
		t.Errorf("stdin = %q, want the file content", got)
	}
}

func TestPushSurfacesFailures(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	d := dialerFor(t, srv, []string{"berry"}, nil)
	c, _ := d.Dial(context.Background(), "n", "127.0.0.1")
	defer c.Close()

	// Fail whatever the push command turns out to be.
	if err := c.Push("/x", []byte("data"), "0644"); err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	cmd := pushCmdFrom(srv.ran())
	if cmd == "" {
		t.Fatalf("no push command ran; got %v", srv.ran())
	}
	srv.fail(cmd, 1)
	if err := c.Push("/x", []byte("data"), "0644"); err == nil {
		t.Error("Push reported success for a failing command")
	}
}

func TestFetchReadsAsRoot(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	srv.reply("sudo -n cat '/boot/firmware/cmdline.txt'", "console=tty1 rootwait\n")
	d := dialerFor(t, srv, []string{"berry"}, nil)
	c, _ := d.Dial(context.Background(), "n", "127.0.0.1")
	defer c.Close()

	got, err := c.Fetch("/boot/firmware/cmdline.txt")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(got) != "console=tty1 rootwait\n" {
		t.Errorf("Fetch = %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/boot/firmware/config.txt": `'/boot/firmware/config.txt'`,
		"it's":                      `'it'\''s'`,
		"":                          `''`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewReadsTheConfiguredKey(t *testing.T) {
	_, pemBytes := testKey(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	pubPath := filepath.Join(dir, "id_ed25519.pub")
	os.WriteFile(pubPath, []byte("ssh-ed25519 AAAA test\n"), 0o644)

	cfg := &config.Config{
		SSH:       config.SSH{Key: keyPath, Users: []string{"berry", "ericovis"}},
		Provision: config.Provision{User: "berry", AuthorizedKeys: pubPath},
	}
	d, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(d.Users) != 2 || len(d.Auth) == 0 {
		t.Errorf("dialer = %+v", d)
	}
	if d.Timeout != DefaultTimeout {
		t.Errorf("timeout = %v", d.Timeout)
	}
}

func TestNewRejectsABadKeyWhenNoAgentCanHelp(t *testing.T) {
	// With no agent, an unusable key file leaves nothing to authenticate
	// with, and the error must say how to fix it.
	t.Setenv("SSH_AUTH_SOCK", "")

	cfg := &config.Config{SSH: config.SSH{Key: filepath.Join(t.TempDir(), "absent"), Users: []string{"berry"}}}
	_, err := New(cfg, nil)
	if err == nil {
		t.Fatal("New accepted a missing key with no agent")
	}
	if !strings.Contains(err.Error(), "ssh-add") {
		t.Errorf("err = %v, want it to suggest ssh-add", err)
	}

	bad := filepath.Join(t.TempDir(), "bad")
	os.WriteFile(bad, []byte("not a key"), 0o600)
	cfg.SSH.Key = bad
	if _, err := New(cfg, nil); err == nil {
		t.Error("New accepted an unparsable key with no agent")
	}
}

func TestNewUsesTheAgentWhenTheKeyFileIsUnusable(t *testing.T) {
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		t.Skip("no SSH agent on this machine")
	}
	if _, err := AgentSigners(); err != nil {
		t.Skipf("agent unusable: %v", err)
	}
	cfg := &config.Config{SSH: config.SSH{
		Key:   filepath.Join(t.TempDir(), "does-not-exist"),
		Users: []string{"berry"},
	}}
	d, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New with only an agent: %v", err)
	}
	if len(d.Auth) != 1 {
		t.Errorf("auth methods = %d, want just the agent", len(d.Auth))
	}
}

func TestDialWithNoAuth(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	d := &Dialer{Users: []string{"berry"}, Port: srv.Port()}
	if _, err := d.Dial(context.Background(), "n", "127.0.0.1"); err == nil {
		t.Error("Dial accepted a dialer with no credentials")
	}
}

func TestDialRespectsContextCancellation(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	d := dialerFor(t, srv, []string{"berry"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Dial(ctx, "n", "127.0.0.1"); err == nil {
		t.Error("Dial ignored a cancelled context")
	}
}

func TestSudoUsesThePasswordlessPathWhenAvailable(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	d := dialerFor(t, srv, []string{"berry"}, nil)
	d.SudoPassword = "hunter2"
	c, err := d.Dial(context.Background(), "n", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Sudo("true"); err != nil {
		t.Fatalf("Sudo: %v", err)
	}
	for _, cmd := range srv.ran() {
		if strings.Contains(cmd, "-S") {
			t.Errorf("used the password path on a node with NOPASSWD: %q", cmd)
		}
		if strings.Contains(cmd, "hunter2") {
			t.Errorf("the password appeared in a command line: %q", cmd)
		}
	}
}

func TestSudoFallsBackToThePasswordPath(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	srv.fail("sudo -n true", 1) // this node has no NOPASSWD
	d := dialerFor(t, srv, []string{"berry"}, nil)
	d.SudoPassword = "hunter2"
	c, err := d.Dial(context.Background(), "n", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Sudo("systemctl reboot"); err != nil {
		t.Fatalf("Sudo: %v", err)
	}
	want := "sudo -k -S -p '' systemctl reboot"
	var found bool
	for _, cmd := range srv.ran() {
		if cmd == want {
			found = true
			// The password goes on stdin, never on the command line.
			if got := srv.stdinFor(cmd); got != "hunter2\n" {
				t.Errorf("stdin = %q, want the password followed by a newline", got)
			}
		}
	}
	if !found {
		t.Errorf("commands were %v, want one to be %q", srv.ran(), want)
	}
}

func TestSudoProbesOnlyOnce(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	srv.fail("sudo -n true", 1)
	d := dialerFor(t, srv, []string{"berry"}, nil)
	d.SudoPassword = "pw"
	c, _ := d.Dial(context.Background(), "n", "127.0.0.1")
	defer c.Close()

	for i := 0; i < 3; i++ {
		if _, err := c.Sudo("true"); err != nil {
			t.Fatalf("Sudo %d: %v", i, err)
		}
	}
	var probes int
	for _, cmd := range srv.ran() {
		if cmd == "sudo -n true" {
			probes++
		}
	}
	if probes != 1 {
		t.Errorf("probed sudo %d times, want 1", probes)
	}
}

func TestSudoWithoutAPasswordExplainsHowToFixIt(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	srv.fail("sudo -n true", 1)
	d := dialerFor(t, srv, []string{"berry"}, nil) // no SudoPassword
	c, _ := d.Dial(context.Background(), "n", "127.0.0.1")
	defer c.Close()

	_, err := c.Sudo("true")
	if err == nil {
		t.Fatal("Sudo succeeded on a node needing a password with none supplied")
	}
	for _, want := range []string{"NOPASSWD", "ssh.sudo: password"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

// TestPushWithSudoPasswordKeepsThePasswordOutOfTheFile guards the trap in
// the password path: sudo -S consumes one line of stdin, and the rest is the
// file content. If sudo skipped that read — because of a cached credential —
// the password itself would be written into the file. `-k` is what prevents
// that, so this test pins both the flag and the stdin framing.
func TestPushWithSudoPasswordKeepsThePasswordOutOfTheFile(t *testing.T) {
	srv := newTestSSHD(t, "berry")
	srv.fail("sudo -n true", 1)
	d := dialerFor(t, srv, []string{"berry"}, nil)
	d.SudoPassword = "hunter2"
	c, _ := d.Dial(context.Background(), "n", "127.0.0.1")
	defer c.Close()

	content := []byte("initramfs recovery.gz followkernel\n")
	if err := c.Push("/boot/firmware/config.txt", content, "0644"); err != nil {
		t.Fatalf("Push: %v", err)
	}

	var pushCmd string
	for _, cmd := range srv.ran() {
		if strings.Contains(cmd, "config.txt") {
			pushCmd = cmd
		}
	}
	if pushCmd == "" {
		t.Fatalf("no push command ran; got %v", srv.ran())
	}
	if !strings.HasPrefix(pushCmd, "sudo -k -S -p '' ") {
		t.Errorf("push command %q must use -k so sudo always consumes the password line", pushCmd)
	}
	if got, want := srv.stdinFor(pushCmd), "hunter2\n"+string(content); got != want {
		t.Errorf("stdin = %q, want the password line then the content", got)
	}
}
