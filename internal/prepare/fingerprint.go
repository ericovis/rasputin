package prepare

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/ericovis/rasputin/internal/bootfs"
	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/provision"
)

// Fingerprint is prepare's idempotency key: a hash of everything from
// rasputin.yaml and the embedded templates that decides what the prepared
// image is — the stock image it starts from, the rendered provision files
// and nodes.conf.
//
// It exists so `rasputin sync` can tell "the prepared image already matches
// this config" from "the config changed". It deliberately covers only what
// the *image* depends on: a comment or a timeout edit in the YAML does not
// change it, and neither does a change to internal/agent (recovery.gz is
// rebuilt by every prepare anyway, so re-preparing for it is cheap and
// explicit — `sync -force-prepare`).
func Fingerprint(cfg *config.Config, baseImage string) (string, error) {
	data, err := provision.NewData(cfg, baseImage)
	if err != nil {
		return "", err
	}
	// PreparedAt is the build host's clock, templated in because a Pi has no
	// RTC. Hashing it would make every fingerprint unique and the whole key
	// useless, so it is cleared here.
	data.PreparedAt = ""

	files, err := provision.Render(data)
	if err != nil {
		return "", err
	}

	pairs := make([][2]string, 0, len(cfg.Nodes))
	for _, n := range cfg.Nodes {
		pairs = append(pairs, [2]string{n.MAC, n.Name})
	}
	files[provision.NodesFile] = bootfs.NodesConf(pairs)

	// image.source_url picks the stock image the whole thing is built on. It
	// reaches none of the rendered files, so it has to be hashed on its own:
	// without it, pointing rasputin.yaml at a new Raspberry Pi OS release
	// would leave `sync` reporting that the prepared image still matches.
	files["image.source_url"] = []byte(cfg.Image.SourceURL)

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		// The length is hashed too, so no pair of files can be shuffled into
		// the same byte stream.
		fmt.Fprintf(h, "%s\n%d\n", name, len(files[name]))
		h.Write(files[name])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
