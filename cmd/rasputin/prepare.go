package main

import (
	"flag"
	"fmt"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/prepare"
)

// runPrepare builds the recovery initramfs and the customised stock image
// that adopt, bake and flash all work from.
func runPrepare(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	initramfsOnly := fs.Bool("initramfs-only", false, "stop after building the recovery initramfs")
	if err := fs.Parse(args); err != nil {
		return err
	}

	meta, err := prepare.Run(cfg, prepare.Options{
		InitramfsOnly: *initramfsOnly,
		Log:           func(format string, a ...any) { fmt.Printf(format+"\n", a...) },
	})
	if err != nil {
		return err
	}

	fmt.Printf("\nbuild %s\n", meta.BuildID)
	fmt.Printf("  recovery:   %s (sha256 %s)\n", meta.RecoveryPath, short(meta.RecoveryHash))
	if *initramfsOnly {
		return nil
	}
	fmt.Printf("  base:       %s\n", meta.BaseImage)
	fmt.Printf("  image:      %s (%d bytes)\n", meta.ImagePath, meta.ImageBytes)
	fmt.Printf("  compressed: %s (%d bytes, sha256 %s)\n", meta.ZstPath, meta.ZstBytes, short(meta.SHA256Zst))
	fmt.Printf("  meta:       %s\n", prepare.MetaPath)
	return nil
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12] + "…"
	}
	return hash
}
