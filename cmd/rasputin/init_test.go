package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericovis/rasputin/internal/config"
)

func TestInitWritesATemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rasputin.yaml")
	var out bytes.Buffer
	if err := initConfig(testOutput(&out), path, nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if _, err := config.Parse(data, path); err == nil {
		t.Error("the file init wrote parsed; placeholder MACs must be refused until edited")
	}
	for _, want := range []string{"nodes:", "rasputin004", "00:00:00:00:00:04", "builder: rasputin001"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("written config is missing %q:\n%s", want, data)
		}
	}
	for _, want := range []string{path, "eth0/address", "rasputin sync"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q, want it to mention %q", out.String(), want)
		}
	}
}

func TestInitNodeFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
		nodes   []config.Node
		builder string
	}{
		{
			name:    "two nodes, builder defaults to the first",
			args:    []string{"-node", "pi1=B8:27:EB:01:02:03", "-node", "pi2=b8:27:eb:04:05:06"},
			nodes:   []config.Node{{Name: "pi1", MAC: "b8:27:eb:01:02:03"}, {Name: "pi2", MAC: "b8:27:eb:04:05:06"}},
			builder: "pi1",
		},
		{
			name:    "explicit builder",
			args:    []string{"-builder", "pi2", "-node", "pi1=b8:27:eb:01:02:03", "-node", "pi2=b8:27:eb:04:05:06"},
			nodes:   []config.Node{{Name: "pi1", MAC: "b8:27:eb:01:02:03"}, {Name: "pi2", MAC: "b8:27:eb:04:05:06"}},
			builder: "pi2",
		},
		{
			name:    "builder that is not a node",
			args:    []string{"-builder", "pi9", "-node", "pi1=b8:27:eb:01:02:03"},
			wantErr: "not a known node",
		},
		{
			// The placeholder config cannot be parsed, so this is the one
			// path where a bad -builder used to reach the disk and only fail
			// once the real MACs had been typed in.
			name:    "builder that is not a node, with the placeholders",
			args:    []string{"-builder", "nope"},
			wantErr: "not a known node",
		},
		{
			name:    "malformed node flag",
			args:    []string{"-node", "pi1"},
			wantErr: "want name=mac",
		},
		{
			name:    "bad mac",
			args:    []string{"-node", "pi1=zz"},
			wantErr: "bad mac",
		},
		{
			name:    "duplicate mac",
			args:    []string{"-node", "pi1=b8:27:eb:01:02:03", "-node", "pi2=b8:27:eb:01:02:03"},
			wantErr: "share mac",
		},
		{
			name:    "stray argument",
			args:    []string{"all"},
			wantErr: "takes no arguments",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rasputin.yaml")
			var out bytes.Buffer
			err := initConfig(testOutput(&out), path, tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				if _, statErr := os.Stat(path); statErr == nil {
					t.Error("a rejected init still wrote the file")
				}
				return
			}
			if err != nil {
				t.Fatalf("init: %v", err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("the written config does not load: %v", err)
			}
			if len(cfg.Nodes) != len(tc.nodes) {
				t.Fatalf("nodes = %+v, want %+v", cfg.Nodes, tc.nodes)
			}
			for i, want := range tc.nodes {
				if cfg.Nodes[i] != want {
					t.Errorf("node %d = %+v, want %+v", i, cfg.Nodes[i], want)
				}
			}
			if cfg.Builder != tc.builder {
				t.Errorf("builder = %q, want %q", cfg.Builder, tc.builder)
			}
			if strings.Contains(out.String(), "eth0/address") {
				t.Errorf("output tells the user to fix MACs it already has: %q", out.String())
			}
		})
	}
}

// TestInitAcceptsAPlaceholderBuilder: the check that rejects an unknown
// -builder must still let a real placeholder node through.
func TestInitAcceptsAPlaceholderBuilder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rasputin.yaml")
	var out bytes.Buffer
	if err := initConfig(testOutput(&out), path, []string{"-builder", "rasputin003"}); err != nil {
		t.Fatalf("init -builder rasputin003: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "builder: rasputin003") {
		t.Errorf("written config does not name the builder:\n%s", data)
	}
}

func TestInitRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rasputin.yaml")
	if err := os.WriteFile(path, []byte("cluster: mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := initConfig(testOutput(&out), path, nil)
	if err == nil {
		t.Fatal("init overwrote an existing config without -force")
	}
	if !strings.Contains(err.Error(), "-force") {
		t.Errorf("err = %v, want it to name the flag that overrides", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "cluster: mine\n" {
		t.Errorf("the existing file changed: %q", data)
	}

	if err := initConfig(testOutput(&out), path, []string{"-force"}); err != nil {
		t.Fatalf("init -force: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "rasputin004") {
		t.Errorf("-force did not overwrite the file:\n%s", data)
	}
}

// TestOnlyInitAndManualRunWithoutAConfig guards the dispatch: every other
// command must still get a loaded, validated config.
func TestOnlyInitAndManualRunWithoutAConfig(t *testing.T) {
	for _, c := range commands {
		exempt := c.name == "init" || c.name == "manual"
		if exempt == c.needsConfig {
			t.Errorf("command %q: needsConfig = %v", c.name, c.needsConfig)
		}
	}
	if commands[0].name != "init" {
		t.Errorf("commands[0] = %q, want init listed first in the usage table", commands[0].name)
	}
}
