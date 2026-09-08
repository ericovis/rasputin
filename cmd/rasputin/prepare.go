package main

import (
	"context"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/prepare"
)

// runPrepare builds the recovery initramfs and the customised stock image
// that adopt, bake and flash all work from.
func runPrepare(cfg *config.Config, out *output, args []string) error {
	fs := out.flagSet("prepare")
	initramfsOnly := fs.Bool("initramfs-only", false, "stop after building the recovery initramfs")
	fs.Usage = func() {
		out.printfErr("usage: rasputin prepare [flags]\n\n"+
			"Builds out/recovery.gz and out/vanilla-custom.img.zst from the stock\n"+
			"image named in %s. Touches only out/ and cache/, never a node.\n"+
			"The raw out/vanilla-custom.img is kept for a Day-0 `dd`.\n\nflags:\n", cfg.Path)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	meta, err := prepare.Run(context.Background(), cfg, prepare.Options{
		InitramfsOnly: *initramfsOnly,
		Log:           out.logf,
	})
	if err != nil {
		return err
	}

	out.printf("\nbuild %s\n", meta.BuildID)
	out.printf("  recovery:   %s (sha256 %s)\n", meta.RecoveryPath, short(meta.RecoveryHash))
	if !*initramfsOnly {
		out.printf("  base:       %s\n", meta.BaseImage)
		out.printf("  image:      %s (%d bytes)\n", meta.ImagePath, meta.ImageBytes)
		out.printf("  compressed: %s (%d bytes, sha256 %s)\n", meta.ZstPath, meta.ZstBytes, short(meta.SHA256Zst))
		out.printf("  meta:       %s\n", prepare.MetaPath)
	}
	return out.result("prepare", struct {
		InitramfsOnly bool   `json:"initramfs_only"`
		MetaPath      string `json:"meta_path"`
		*prepare.Meta
	}{*initramfsOnly, prepare.MetaPath, meta}, nil)
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12] + "…"
	}
	return hash
}
