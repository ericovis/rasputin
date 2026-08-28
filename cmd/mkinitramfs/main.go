// Command mkinitramfs builds out/recovery.gz from the current checkout.
//
// It is the Makefile's `build` step and is kept separate from the CLI so that
// building the initramfs never depends on a valid cluster config or a
// reachable network.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ericovis/rasputin/internal/initramfs"
)

func main() {
	out := flag.String("o", "out/recovery.gz", "output path for the packed initramfs")
	url := flag.String("url", "", "default image URL baked into the agent (optional)")
	version := flag.String("version", "dev", "version string stamped into the agent")
	repo := flag.String("C", ".", "module root to build from")
	flag.Parse()

	res, err := initramfs.Build(initramfs.Options{
		RepoDir:    *repo,
		OutPath:    *out,
		DefaultURL: *url,
		Version:    *version,
		Log:        func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkinitramfs: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("recovery initramfs ready: %s (%d bytes)\n", res.OutPath, res.CompressedSize)
}
