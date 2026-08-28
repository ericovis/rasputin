package main

import (
	"fmt"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/vanilla"
)

// runVanillaFetch downloads and caches the stock image. `prepare` does this
// as its first step; the standalone command exists so the ~1 GB download can
// be done (and verified) on its own.
func runVanillaFetch(cfg *config.Config, args []string) error {
	path, meta, err := vanilla.Ensure(vanilla.Options{
		SourceURL: cfg.Image.SourceURL,
		Log:       func(format string, a ...any) { fmt.Printf(format+"\n", a...) },
	})
	if err != nil {
		return err
	}
	fmt.Printf("vanilla image: %s\n", path)
	fmt.Printf("  resolved:   %s\n", meta.ResolvedURL)
	fmt.Printf("  size:       %d bytes (%.2f GiB)\n", meta.Bytes, float64(meta.Bytes)/(1<<30))
	fmt.Printf("  sha256:     %s\n", meta.SHA256Img)
	fmt.Printf("  meta:       %s\n", vanilla.DefaultMetaPath)
	return nil
}
