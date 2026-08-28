// Package config loads and validates rasputin.yaml.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Defaults applied when a field is absent from the YAML.
const (
	DefaultPort         = 8080
	DefaultRootfsSizeGB = 8
	DefaultFlashMinutes = 25
	DefaultBakeMinutes  = 45
	DefaultCluster      = "rasputin"
)

// Config mirrors rasputin.yaml.
type Config struct {
	Cluster   string    `yaml:"cluster"`
	Server    Server    `yaml:"server"`
	Image     Image     `yaml:"image"`
	SSH       SSH       `yaml:"ssh"`
	Provision Provision `yaml:"provision"`
	Builder   string    `yaml:"builder"`
	Timeouts  Timeouts  `yaml:"timeouts"`
	Nodes     []Node    `yaml:"nodes"`

	// Path is the file this config was loaded from (not part of the YAML).
	Path string `yaml:"-"`
}

// Server is the built-in HTTP server configuration. An explicit port of 0
// means "pick a free port"; an absent key means DefaultPort. The bind IP is
// always auto-detected at runtime, never configured.
type Server struct {
	Port *int `yaml:"port"`
}

// ListenPort is the port to bind, with the default already applied.
func (s Server) ListenPort() int {
	if s.Port == nil {
		return DefaultPort
	}
	return *s.Port
}

// Image describes where the vanilla Raspberry Pi OS image comes from and how
// large the provisioned rootfs should end up.
type Image struct {
	SourceURL    string `yaml:"source_url"`
	RootfsSizeGB int    `yaml:"rootfs_size_gb"`
}

// SSH holds the credentials the CLI uses to reach nodes. Users are tried in
// order, so a half-migrated cluster keeps working.
type SSH struct {
	Key   string   `yaml:"key"`
	Users []string `yaml:"users"`
}

// Provision describes the state firstrun.sh bakes into the golden image.
type Provision struct {
	User           string   `yaml:"user"`
	AuthorizedKeys string   `yaml:"authorized_keys"`
	Timezone       string   `yaml:"timezone"`
	Locale         string   `yaml:"locale"`
	Packages       []string `yaml:"packages"`
}

// Timeouts bounds the long-running hardware operations.
type Timeouts struct {
	FlashMinutes int `yaml:"flash_minutes"`
	BakeMinutes  int `yaml:"bake_minutes"`
}

// Node is one Raspberry Pi. MAC is the eth0 address, used to map an identity
// onto a byte-identical golden clone.
type Node struct {
	Name string `yaml:"name"`
	MAC  string `yaml:"mac"`
}

// Load reads, defaults and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, path)
}

// Parse decodes YAML bytes into a validated Config. path is recorded for
// diagnostics and may be empty.
func Parse(data []byte, path string) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.Path = path
	c.applyDefaults()
	if err := c.normalize(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Cluster == "" {
		c.Cluster = DefaultCluster
	}
	if c.Server.Port == nil {
		p := DefaultPort
		c.Server.Port = &p
	}
	if c.Image.RootfsSizeGB == 0 {
		c.Image.RootfsSizeGB = DefaultRootfsSizeGB
	}
	if c.Timeouts.FlashMinutes == 0 {
		c.Timeouts.FlashMinutes = DefaultFlashMinutes
	}
	if c.Timeouts.BakeMinutes == 0 {
		c.Timeouts.BakeMinutes = DefaultBakeMinutes
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SSH.Key, err = expandUser(c.SSH.Key); err != nil {
		return err
	}
	if c.Provision.AuthorizedKeys, err = expandUser(c.Provision.AuthorizedKeys); err != nil {
		return err
	}
	for i := range c.Nodes {
		mac, perr := net.ParseMAC(c.Nodes[i].MAC)
		if perr != nil {
			return fmt.Errorf("node %s: bad mac %q: %w", c.Nodes[i].Name, c.Nodes[i].MAC, perr)
		}
		c.Nodes[i].MAC = strings.ToLower(mac.String())
	}
	return nil
}

// Validate checks the invariants the rest of the tool relies on.
func (c *Config) Validate() error {
	if len(c.Nodes) == 0 {
		return fmt.Errorf("config: at least one node is required")
	}
	seen := map[string]string{}
	for _, n := range c.Nodes {
		if n.Name == "" {
			return fmt.Errorf("config: node with empty name")
		}
		if prev, dup := seen[n.MAC]; dup {
			return fmt.Errorf("config: nodes %s and %s share mac %s", prev, n.Name, n.MAC)
		}
		seen[n.MAC] = n.Name
	}
	if c.Builder == "" {
		return fmt.Errorf("config: builder is required")
	}
	if c.Node(c.Builder) == nil {
		return fmt.Errorf("config: builder %q is not a known node", c.Builder)
	}
	if c.Provision.User == "" {
		return fmt.Errorf("config: provision.user is required")
	}
	if len(c.SSH.Users) == 0 {
		return fmt.Errorf("config: ssh.users must list at least one user")
	}
	for _, u := range c.SSH.Users {
		if u == "" {
			return fmt.Errorf("config: ssh.users contains an empty user")
		}
	}
	if c.SSH.Key == "" {
		return fmt.Errorf("config: ssh.key is required")
	}
	if p := c.Server.ListenPort(); p < 0 || p > 65535 {
		return fmt.Errorf("config: server.port %d out of range 0-65535", p)
	}
	if c.Image.SourceURL == "" {
		return fmt.Errorf("config: image.source_url is required")
	}
	if c.Image.RootfsSizeGB <= 0 {
		return fmt.Errorf("config: image.rootfs_size_gb must be positive")
	}
	if c.Timeouts.FlashMinutes <= 0 || c.Timeouts.BakeMinutes <= 0 {
		return fmt.Errorf("config: timeouts must be positive")
	}
	return nil
}

// Node returns the node with the given name, or nil.
func (c *Config) Node(name string) *Node {
	for i := range c.Nodes {
		if c.Nodes[i].Name == name {
			return &c.Nodes[i]
		}
	}
	return nil
}

// NodeNames lists node names in config order.
func (c *Config) NodeNames() []string {
	names := make([]string, len(c.Nodes))
	for i, n := range c.Nodes {
		names[i] = n.Name
	}
	return names
}

func expandUser(p string) (string, error) {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p, nil
	}
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return "", fmt.Errorf("config: cannot expand %q (only ~/ is supported)", p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}
