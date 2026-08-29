// Package cluster is the orchestration layer: it drives real nodes over SSH
// and the built-in HTTP server, and is what the CLI commands are made of.
package cluster

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/nodes"
	"github.com/ericovis/rasputin/internal/prepare"
	"github.com/ericovis/rasputin/internal/server"
	"github.com/ericovis/rasputin/internal/sshx"
	"github.com/ericovis/rasputin/internal/state"
)

// Paths on a node's boot partition.
const (
	BootMount    = nodes.BootMount
	RecoveryPath = BootMount + "/recovery.gz"
	ConfigPath   = BootMount + "/config.txt"
	CmdlinePath  = BootMount + "/cmdline.txt"
	NodesPath    = BootMount + "/nodes.conf"
	BackupSuffix = ".pre-rasputin"
)

// Flag files, mirroring internal/agent's names.
const (
	FlagReflash = BootMount + "/reflash"
	FlagDryrun  = BootMount + "/reflash-dryrun"
	FlagCapture = BootMount + "/capture"
	DryrunLog   = BootMount + "/reflash-dryrun.log"
)

// AllNodes selects the whole cluster, mirroring nodes.AllKeyword.
const AllNodes = nodes.AllKeyword

// GoneTimeout is how long to wait for a node to actually go down after a
// reboot is requested. Waiting for it matters: without it, a reboot that
// silently failed would look like an instantly successful one.
const GoneTimeout = 3 * time.Minute

// Cluster holds everything the commands share.
type Cluster struct {
	Cfg      *config.Config
	State    *state.Store
	Resolver *nodes.Resolver
	Log      func(format string, args ...any)
}

// New wires up the state cache, the SSH dialer and the node resolver.
func New(cfg *config.Config, logf func(format string, args ...any)) (*Cluster, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	st, err := state.Load(state.DefaultPath)
	if err != nil {
		return nil, err
	}
	dialer, err := sshx.New(cfg, st)
	if err != nil {
		return nil, err
	}
	return &Cluster{
		Cfg:   cfg,
		State: st,
		Log:   logf,
		Resolver: &nodes.Resolver{
			Cfg:   cfg,
			State: st,
			Dial:  nodes.Adapt(dialer),
			Log:   logf,
		},
	}, nil
}

// Serve starts the HTTP server nodes download from and upload to.
func (c *Cluster) Serve() (*server.Server, error) {
	return server.Start(server.Options{
		Port:   c.Cfg.Server.ListenPort(),
		OutDir: prepare.OutDir,
		Log:    c.Log,
	})
}

// Image names a file the server can offer to nodes.
type Image struct {
	Name string
	Path string
}

// GoldenImage is the byte-identical clone every flash uses.
var GoldenImage = Image{Name: "golden.img.zst", Path: "out/golden.img.zst"}

// VanillaImage is the prepared stock image, used to bake the golden one and
// as the dryrun payload before a golden image exists.
var VanillaImage = Image{Name: "vanilla-custom.img.zst", Path: prepare.ImageZstPath}

// Exists reports whether the image has been built.
func (i Image) Exists() bool {
	info, err := os.Stat(i.Path)
	return err == nil && info.Size() > 0
}

// BestImage prefers the golden image and falls back to the prepared stock
// one, which is what a dryrun wants before the first bake.
func BestImage() (Image, error) {
	if GoldenImage.Exists() {
		return GoldenImage, nil
	}
	if VanillaImage.Exists() {
		return VanillaImage, nil
	}
	return Image{}, fmt.Errorf("no image to serve: run `rasputin prepare` first")
}

// Reboot asks a node to restart.
//
// The connection dies as the node goes down, so a transport error here is
// expected, not a failure; whether the reboot worked is decided by watching
// the node disappear and come back.
func Reboot(conn nodes.Conn) error {
	if _, err := conn.Sudo("systemctl --no-block reboot"); err == nil {
		return nil
	}
	if _, err := conn.Sudo("sh -c 'sleep 1; reboot' >/dev/null 2>&1 &"); err == nil {
		return nil
	}
	// Last resort, and the one most likely to kill the session mid-reply.
	_, err := conn.Sudo("reboot")
	if err != nil && isDisconnect(err) {
		return nil
	}
	return err
}

func isDisconnect(err error) bool {
	s := err.Error()
	for _, marker := range []string{"EOF", "closed", "connection reset", "broken pipe"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// RebootOptions configures RebootAndWait.
type RebootOptions struct {
	// Back is how long the node has to return.
	Back time.Duration
	// ReplacesSystem says the node will come back as a different install,
	// with freshly generated SSH host keys. The pinned key must then be
	// dropped — but only once the node is actually down. Clearing it any
	// earlier is useless: the polling that watches for the node to go away
	// opens SSH connections of its own, and the first of those re-pins the
	// key that is about to be destroyed.
	ReplacesSystem bool
}

// RebootAndWait restarts a node and returns a fresh connection once it is
// back. It first waits for the node to actually go away, so a reboot that
// never happened is reported as such instead of passing silently.
func (c *Cluster) RebootAndWait(ctx context.Context, node config.Node, conn nodes.Conn, opts RebootOptions) (nodes.Conn, error) {
	c.Log("%s: rebooting", node.Name)
	if err := Reboot(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%s: cannot reboot: %w", node.Name, err)
	}
	conn.Close()

	if err := c.waitGone(ctx, node); err != nil {
		return nil, err
	}
	if opts.ReplacesSystem {
		// The node is down; nothing can re-pin the old key from here.
		if err := c.State.ForgetHostKey(node.Name); err != nil {
			c.Log("%s: could not clear the recorded host key: %v", node.Name, err)
		}
	}
	c.Log("%s: down, waiting for it to come back (up to %s)", node.Name, opts.Back)
	return c.Resolver.WaitFor(ctx, node, opts.Back)
}

// waitGone blocks until a node stops answering SSH.
func (c *Cluster) waitGone(ctx context.Context, node config.Node) error {
	deadline := time.Now().Add(GoneTimeout)
	for {
		if !c.Resolver.Reachable(ctx, node) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s never went down after a reboot was requested", node.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// WriteFlag drops a flag file on a node's boot partition, with the URL the
// agent should use as its first line.
func WriteFlag(conn nodes.Conn, path, url string) error {
	return conn.Push(path, []byte(url+"\n"), "0644")
}

// RemoteFile reads a file from a node, returning "" when it does not exist.
func RemoteFile(conn nodes.Conn, path string) (string, error) {
	res, err := conn.Run("sudo -n cat " + shellQuote(path) + " 2>/dev/null || true")
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ProgressLine renders a node's download progress for the console.
func ProgressLine(p server.Progress) string {
	if p.Total <= 0 {
		return fmt.Sprintf("%d MiB", p.Bytes>>20)
	}
	return fmt.Sprintf("%d/%d MiB (%.0f%%, %.1f MB/s)",
		p.Bytes>>20, p.Total>>20, p.Percent(), p.Rate()/1e6)
}
