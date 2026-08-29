// Package sshx is the CLI's SSH client.
//
// It exists rather than shelling out to `ssh` because the CLI needs three
// things the command line makes awkward: trying several usernames until one
// authenticates (the cluster is migrated node by node from `ericovis` to
// `berry`), recording each node's host key in our own cache, and pushing
// files as root without depending on sftp being installed.
package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/ericovis/rasputin/internal/config"
)

// DefaultTimeout bounds a dial. Nodes are on the LAN; anything slower than
// this is down, not busy.
const DefaultTimeout = 10 * time.Second

// Port is the SSH port. Raspberry Pi OS does not move it and neither do we.
const Port = 22

// HostKeyStore records the host key each node presents, so a changed key is
// noticed. internal/state implements it.
type HostKeyStore interface {
	HostKey(node string) string
	SetHostKey(node, key string) error
}

// Dialer opens connections to nodes.
type Dialer struct {
	// Users are tried in order until one authenticates.
	Users []string
	// Auth is offered on every attempt, in order.
	Auth []ssh.AuthMethod
	// Timeout bounds each dial.
	Timeout time.Duration
	// Keys, when set, records and checks host keys.
	Keys HostKeyStore
	// Port overrides the SSH port; 0 means Port (22). Only tests set it.
	Port int
	// SudoPassword, when set, lets Sudo fall back to `sudo -S` on nodes
	// that have not been granted passwordless sudo. Empty means the CLI
	// requires NOPASSWD, which is the default.
	SudoPassword string
}

// New builds a Dialer from the cluster config.
//
// Two ways in are offered, in this order: the running SSH agent, and the key
// file itself. The agent comes first because the owner's key is passphrase
// protected — asking for a passphrase on every one of the dozens of
// connections a flash makes would be unusable, and the agent already holds
// the unlocked key.
func New(cfg *config.Config, keys HostKeyStore) (*Dialer, error) {
	var (
		auth     []ssh.AuthMethod
		problems []string
	)

	if signers, err := AgentSigners(); err != nil {
		problems = append(problems, err.Error())
	} else if len(signers) > 0 {
		auth = append(auth, ssh.PublicKeysCallback(AgentSigners))
	}

	if raw, err := os.ReadFile(cfg.SSH.Key); err != nil {
		problems = append(problems, fmt.Sprintf("reading %s: %v", cfg.SSH.Key, err))
	} else if signer, err := ssh.ParsePrivateKey(raw); err != nil {
		problems = append(problems, fmt.Sprintf("%s: %v", cfg.SSH.Key, err))
	} else {
		auth = append(auth, ssh.PublicKeys(signer))
	}

	if len(auth) == 0 {
		return nil, fmt.Errorf("no usable SSH credentials:\n  %s\n"+
			"If the key is passphrase protected, load it into the agent: ssh-add %s",
			strings.Join(problems, "\n  "), cfg.SSH.Key)
	}
	return &Dialer{
		Users:   append([]string(nil), cfg.SSH.Users...),
		Auth:    auth,
		Timeout: DefaultTimeout,
		Keys:    keys,
	}, nil
}

// WithSudoPassword returns d with a sudo password attached.
func (d *Dialer) WithSudoPassword(pw string) *Dialer {
	d.SudoPassword = pw
	return d
}

// AgentSigners returns the keys held by the running SSH agent, or an error
// explaining why there are none.
func AgentSigners() ([]ssh.Signer, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, fmt.Errorf("no SSH agent (SSH_AUTH_SOCK is not set)")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("connecting to the SSH agent at %s: %w", sock, err)
	}
	// The connection is deliberately not closed: ssh.PublicKeysCallback
	// calls this on every handshake, and each call opens its own.
	signers, err := agent.NewClient(conn).Signers()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("listing agent keys: %w", err)
	}
	if len(signers) == 0 {
		conn.Close()
		return nil, fmt.Errorf("the SSH agent holds no keys (try: ssh-add)")
	}
	return signers, nil
}

// Client is one open connection.
type Client struct {
	conn *ssh.Client
	user string
	host string
	node string

	sudoPassword string
	// sudoOnce guards the one-time probe of whether this node needs a
	// password; the answer cannot change during a session.
	sudoOnce    sync.Once
	sudoNeedsPw bool
	sudoErr     error
}

// User is the account that authenticated.
func (c *Client) User() string { return c.user }

// Host is the address that was dialled.
func (c *Client) Host() string { return c.host }

// Node is the logical name this connection was opened for.
func (c *Client) Node() string { return c.node }

// Dial connects to host, trying each configured user in order. node names
// the machine for host-key bookkeeping and may be empty for a throwaway
// connection.
func (d *Dialer) Dial(ctx context.Context, node, host string) (*Client, error) {
	if len(d.Users) == 0 {
		return nil, fmt.Errorf("no SSH users configured")
	}
	if len(d.Auth) == 0 {
		return nil, fmt.Errorf("no SSH credentials configured")
	}
	timeout := d.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	port := d.Port
	if port == 0 {
		port = Port
	}
	addr := net.JoinHostPort(host, fmt.Sprint(port))

	var errs []string
	for _, user := range d.Users {
		cfg := &ssh.ClientConfig{
			User:            user,
			Auth:            d.Auth,
			HostKeyCallback: d.hostKeyCallback(node),
			Timeout:         timeout,
		}
		conn, err := dialContext(ctx, addr, cfg, timeout)
		if err == nil {
			return &Client{conn: conn, user: user, host: host, node: node, sudoPassword: d.SudoPassword}, nil
		}
		errs = append(errs, fmt.Sprintf("%s@%s: %v", user, host, err))
		// A host-key problem is the same for every user, and a wrong key is
		// something an operator must resolve, not something to retry.
		if isHostKeyError(err) {
			break
		}
	}
	return nil, fmt.Errorf("cannot connect to %s: %s", host, strings.Join(errs, "; "))
}

// dialContext dials with the context's cancellation respected.
func dialContext(ctx context.Context, addr string, cfg *ssh.ClientConfig, timeout time.Duration) (*ssh.Client, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// Clear the handshake deadline: a flash streams for minutes.
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

// hostKeyError marks a refusal caused by a changed host key.
type hostKeyError struct{ error }

func isHostKeyError(err error) bool {
	var hk hostKeyError
	return errors.As(err, &hk)
}

// hostKeyCallback accepts a node's key the first time and pins it after
// that. Trust on first use is the right trade here: the cluster is on a
// private LAN, there is no out-of-band channel to learn the keys from, and
// the alternative — accepting any key silently — would hide a real change.
func (d *Dialer) hostKeyCallback(node string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		presented := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if d.Keys == nil || node == "" {
			return nil
		}
		known := d.Keys.HostKey(node)
		if known == "" {
			return d.Keys.SetHostKey(node, presented)
		}
		if known != presented {
			return hostKeyError{fmt.Errorf(
				"host key for %s changed (this is expected after a reflash — the CLI clears the "+
					"recorded key when it flashes; if you did not flash this node, investigate)", node)}
		}
		return nil
	}
}

// Close ends the connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Result is the outcome of one remote command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes a command and returns its output. A non-zero exit is an
// error, with the exit code available on the Result.
func (c *Client) Run(cmd string) (Result, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	runErr := sess.Run(cmd)
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}

	var exitErr *ssh.ExitError
	if errors.As(runErr, &exitErr) {
		res.ExitCode = exitErr.ExitStatus()
		return res, fmt.Errorf("%s@%s: %q exited %d: %s",
			c.user, c.host, cmd, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if runErr != nil {
		return res, fmt.Errorf("%s@%s: running %q: %w", c.user, c.host, cmd, runErr)
	}
	return res, nil
}

// Sudo runs a command as root.
//
// The fast path is passwordless: `sudo -n` fails immediately rather than
// hanging on a prompt nobody can answer. If the node has not been granted
// NOPASSWD and a sudo password was supplied, it falls back to feeding the
// password on stdin instead.
func (c *Client) Sudo(cmd string) (Result, error) {
	needsPw, err := c.sudoNeedsPassword()
	if err != nil {
		return Result{}, err
	}
	if !needsPw {
		return c.Run("sudo -n " + cmd)
	}
	return c.runSudoWithPassword(cmd, nil)
}

// sudoNeedsPassword probes once whether this node accepts passwordless sudo.
func (c *Client) sudoNeedsPassword() (bool, error) {
	c.sudoOnce.Do(func() {
		if _, err := c.Run("sudo -n true"); err == nil {
			c.sudoNeedsPw = false
			return
		}
		if c.sudoPassword == "" {
			c.sudoErr = fmt.Errorf(
				"%s@%s: sudo needs a password and none was supplied. Either grant passwordless sudo "+
					"(echo '%s ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/%s) or set ssh.sudo: password "+
					"in rasputin.yaml", c.user, c.host, c.user, c.user)
			return
		}
		c.sudoNeedsPw = true
	})
	return c.sudoNeedsPw, c.sudoErr
}

// runSudoWithPassword feeds the sudo password on stdin, followed by any data
// the command itself should read.
//
// `-k` discards any cached credential so sudo ALWAYS consumes exactly one
// line from stdin. Without it, a cached timestamp would make sudo skip the
// read and the password line would be delivered to the command as data —
// which, for a file push, means writing the password into the file.
// `-p ”` keeps the prompt off stderr.
func (c *Client) runSudoWithPassword(cmd string, data []byte) (Result, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer sess.Close()

	stdin := make([]byte, 0, len(c.sudoPassword)+1+len(data))
	stdin = append(stdin, c.sudoPassword...)
	stdin = append(stdin, '\n')
	stdin = append(stdin, data...)

	var stdout, stderr bytes.Buffer
	sess.Stdin = bytes.NewReader(stdin)
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	full := "sudo -k -S -p '' " + cmd
	runErr := sess.Run(full)
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}

	var exitErr *ssh.ExitError
	if errors.As(runErr, &exitErr) {
		res.ExitCode = exitErr.ExitStatus()
		msg := strings.TrimSpace(res.Stderr)
		if strings.Contains(msg, "incorrect password") || strings.Contains(msg, "Sorry, try again") {
			return res, fmt.Errorf("%s@%s: the sudo password was rejected", c.user, c.host)
		}
		return res, fmt.Errorf("%s@%s: %q exited %d: %s", c.user, c.host, cmd, res.ExitCode, msg)
	}
	if runErr != nil {
		return res, fmt.Errorf("%s@%s: running %q: %w", c.user, c.host, cmd, runErr)
	}
	return res, nil
}

// Output runs a command and returns its trimmed stdout.
func (c *Client) Output(cmd string) (string, error) {
	res, err := c.Run(cmd)
	return strings.TrimSpace(res.Stdout), err
}

// Push writes data to a remote path as root, creating parent directories.
//
// It pipes through `sudo tee` rather than using sftp: sftp would need the
// subsystem enabled and would still not write to root-owned paths, while
// tee works on any stock Raspberry Pi OS.
func (c *Client) Push(path string, data []byte, mode string) error {
	needsPw, err := c.sudoNeedsPassword()
	if err != nil {
		return err
	}
	if needsPw {
		dir := path[:strings.LastIndex(path, "/")+1]
		inner := shellQuote(fmt.Sprintf("mkdir -p %s && cat > %s && chmod %s %s && sync",
			shellQuote(dir), shellQuote(path), mode, shellQuote(path)))
		if _, err := c.runSudoWithPassword("sh -c "+inner, data); err != nil {
			return fmt.Errorf("pushing %s: %w", path, err)
		}
		return nil
	}

	sess, err := c.conn.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	var stderr bytes.Buffer
	sess.Stdin = bytes.NewReader(data)
	sess.Stderr = &stderr
	sess.Stdout = nil

	dir := path[:strings.LastIndex(path, "/")+1]
	inner := shellQuote(fmt.Sprintf("mkdir -p %s && cat > %s && chmod %s %s && sync",
		shellQuote(dir), shellQuote(path), mode, shellQuote(path)))
	if err := sess.Run("sudo -n sh -c " + inner); err != nil {
		return fmt.Errorf("pushing %s to %s@%s: %w: %s", path, c.user, c.host, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Fetch reads a remote file, using sudo so root-owned files are readable.
func (c *Client) Fetch(path string) ([]byte, error) {
	res, err := c.Sudo("cat " + shellQuote(path))
	if err != nil {
		return nil, err
	}
	return []byte(res.Stdout), nil
}

// shellQuote wraps s in single quotes for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
