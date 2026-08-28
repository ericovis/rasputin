// Package state persists what the CLI has learned about the cluster between
// runs: where each node was last reached, which user answered, and the SSH
// host key it presented.
//
// None of it is authoritative — every fact is re-verified on use — but it
// turns "scan the LAN" into "try the address that worked last time", which
// is the difference between a flash starting in one second and in thirty.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultPath is where the cache lives. It is gitignored along with out/.
const DefaultPath = "out/state.json"

// Node is what we remember about one machine.
type Node struct {
	Name     string    `json:"name"`
	MAC      string    `json:"mac,omitempty"`
	IP       string    `json:"ip,omitempty"`
	SSHUser  string    `json:"ssh_user,omitempty"`
	HostKey  string    `json:"host_key,omitempty"`
	Hostname string    `json:"hostname,omitempty"`
	BuildID  string    `json:"build_id,omitempty"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

type data struct {
	Nodes map[string]Node `json:"nodes"`
}

// Store is a concurrency-safe view of the cache file.
type Store struct {
	path string
	mu   sync.Mutex
	d    data
}

// Load reads the cache, returning an empty store if the file does not exist.
// A corrupt file is also treated as empty: the cache is an optimisation, and
// refusing to run because of it would be worse than rebuilding it.
func Load(path string) (*Store, error) {
	if path == "" {
		path = DefaultPath
	}
	s := &Store{path: path, d: data{Nodes: map[string]Node{}}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var d data
	if err := json.Unmarshal(raw, &d); err != nil {
		return s, nil
	}
	if d.Nodes != nil {
		s.d = d
	}
	return s, nil
}

// Path is the file this store reads and writes.
func (s *Store) Path() string { return s.path }

// Get returns what is known about a node.
func (s *Store) Get(name string) (Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.d.Nodes[name]
	return n, ok
}

// Update mutates one node's record and saves the file.
func (s *Store) Update(name string, fn func(*Node)) error {
	s.mu.Lock()
	n := s.d.Nodes[name]
	n.Name = name
	fn(&n)
	s.d.Nodes[name] = n
	s.mu.Unlock()
	return s.Save()
}

// Seen records a successful contact.
func (s *Store) Seen(name, ip, user string) error {
	return s.Update(name, func(n *Node) {
		n.IP = ip
		n.SSHUser = user
		n.LastSeen = time.Now().UTC()
	})
}

// HostKey returns the recorded host key for a node, or "".
func (s *Store) HostKey(name string) string {
	n, _ := s.Get(name)
	return n.HostKey
}

// SetHostKey records the host key a node presented.
func (s *Store) SetHostKey(name, key string) error {
	return s.Update(name, func(n *Node) { n.HostKey = key })
}

// ForgetHostKey drops a recorded key. Reflashing a node regenerates its host
// keys by design, so every command that reflashes calls this first;
// otherwise the next connection would look exactly like an impersonation.
func (s *Store) ForgetHostKey(name string) error {
	return s.Update(name, func(n *Node) { n.HostKey = "" })
}

// All returns every remembered node.
func (s *Store) All() map[string]Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Node, len(s.d.Nodes))
	for k, v := range s.d.Nodes {
		out[k] = v
	}
	return out
}

// Save writes the cache atomically.
func (s *Store) Save() error {
	s.mu.Lock()
	raw, err := json.MarshalIndent(s.d, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("saving %s: %w", s.path, err)
	}
	return nil
}
