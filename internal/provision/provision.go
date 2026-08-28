// Package provision renders the scripts and systemd units that turn a stock
// Raspberry Pi OS image into a rasputin node.
//
// Nothing here touches the ext4 root filesystem from the build host. These
// files are written to the FAT boot partition, and firstrun.sh — run once by
// systemd on the node's first boot — installs them into the running system.
package provision

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/ericovis/rasputin/internal/config"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// File names as written to the boot partition. firstrun.sh reads the others
// from there and installs them into the root filesystem.
const (
	FirstrunFile         = "firstrun.sh"
	IdentityFile         = "rasputin-identity"
	IdentityServiceFile  = "rasputin-identity.service"
	ProvisionServiceFile = "rasputin-provision.service"
	SealFile             = "rasputin-seal"
	BuildIDFile          = "rasputin-build-id"
	NodesFile            = "nodes.conf"
)

// Data is what the templates are rendered against.
type Data struct {
	User           string
	AuthorizedKeys string
	Timezone       string
	Locale         string
	Packages       []string
	RootfsSizeGB   int
	// BaseImage names the stock image this was built from, recorded in
	// /etc/rasputin-release so a node can say where it came from.
	BaseImage string
}

// PackageList is the space-separated package set, for apt and for logging.
func (d Data) PackageList() string { return strings.Join(d.Packages, " ") }

// NewData builds the template inputs from the cluster config, reading the
// public key file named there.
func NewData(cfg *config.Config, baseImage string) (Data, error) {
	keys, err := os.ReadFile(cfg.Provision.AuthorizedKeys)
	if err != nil {
		return Data{}, fmt.Errorf("reading %s: %w", cfg.Provision.AuthorizedKeys, err)
	}
	trimmed := strings.TrimSpace(string(keys))
	if trimmed == "" {
		return Data{}, fmt.Errorf("%s is empty; a node with no authorized key would be unreachable", cfg.Provision.AuthorizedKeys)
	}
	return Data{
		User:           cfg.Provision.User,
		AuthorizedKeys: trimmed,
		Timezone:       cfg.Provision.Timezone,
		Locale:         cfg.Provision.Locale,
		Packages:       cfg.Provision.Packages,
		RootfsSizeGB:   cfg.Image.RootfsSizeGB,
		BaseImage:      baseImage,
	}, nil
}

// Rendered holds every generated file, keyed by its boot-partition name.
type Rendered map[string][]byte

// templateFor maps a boot-partition file name to the template that makes it.
var templateFor = map[string]string{
	FirstrunFile:         "firstrun.sh.tmpl",
	IdentityFile:         "identity.sh.tmpl",
	IdentityServiceFile:  "identity.service.tmpl",
	ProvisionServiceFile: "provision.service.tmpl",
	SealFile:             "seal.sh.tmpl",
}

// Render produces all the provisioning files for one config.
func Render(d Data) (Rendered, error) {
	out := Rendered{}
	for name, tmpl := range templateFor {
		data, err := render(tmpl, d)
		if err != nil {
			return nil, err
		}
		out[name] = data
	}
	return out, nil
}

// RenderOne renders a single template by its file name, e.g. "seal.sh.tmpl".
func RenderOne(tmplName string, d Data) ([]byte, error) { return render(tmplName, d) }

func render(name string, d Data) ([]byte, error) {
	// missingkey=error turns a typo in a template into a build failure
	// rather than a script with an empty username in it.
	t, err := template.New(name).Option("missingkey=error").
		ParseFS(templateFS, "templates/"+name)
	if err != nil {
		return nil, fmt.Errorf("parsing template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("rendering template %s: %w", name, err)
	}
	return buf.Bytes(), nil
}
