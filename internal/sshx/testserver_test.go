package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testSSHD is a minimal in-process SSH server. It exists so the parts of
// this package that are easy to get subtly wrong — trying several users,
// pinning host keys, quoting a pushed file — are tested against a real
// handshake rather than a mock.
type testSSHD struct {
	Addr     string
	HostKey  ssh.PublicKey
	AcceptAs string // the only username that authenticates

	mu       sync.Mutex
	Commands []string          // every command executed, in order
	Stdin    map[string]string // command -> what was piped into it
	Replies  map[string]string // command -> stdout
	Fails    map[string]int    // command -> exit status

	listener net.Listener
}

func newTestSSHD(t *testing.T, acceptAs string) *testSSHD {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testSSHD{
		Addr:     ln.Addr().String(),
		HostKey:  signer.PublicKey(),
		AcceptAs: acceptAs,
		Stdin:    map[string]string{},
		Replies:  map[string]string{},
		Fails:    map[string]int{},
		listener: ln,
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() != s.AcceptAs {
				return nil, fmt.Errorf("no such user %q", c.User())
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(signer)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn, cfg)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

// Port is the listener's port, for building a Dialer that reaches it.
func (s *testSSHD) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

func (s *testSSHD) handle(nc net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go s.session(ch, chReqs)
	}
}

func (s *testSSHD) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			req.Reply(false, nil)
			continue
		}
		req.Reply(true, nil)

		stdin, _ := io.ReadAll(ch)

		s.mu.Lock()
		s.Commands = append(s.Commands, payload.Command)
		s.Stdin[payload.Command] = string(stdin)
		reply, hasReply := s.Replies[payload.Command]
		status, fails := s.Fails[payload.Command]
		s.mu.Unlock()

		if hasReply {
			io.WriteString(ch, reply)
		}
		if fails {
			fmt.Fprintf(ch.Stderr(), "command failed\n")
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
			return
		}
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		return
	}
}

func (s *testSSHD) ran() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.Commands...)
}

func (s *testSSHD) stdinFor(cmd string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Stdin[cmd]
}

func (s *testSSHD) reply(cmd, out string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Replies[cmd] = out
}

func (s *testSSHD) fail(cmd string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Fails[cmd] = status
}

// testKey generates a client key and returns it as an ssh.Signer plus its
// PEM encoding, for tests that go through New().
func testKey(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(block)
}

// hostKeyString renders a host key the way the store records it.
func hostKeyString(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}
