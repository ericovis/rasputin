package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// runServe runs the HTTP server on its own, for debugging a node that is
// downloading (or refusing to download) an image.
func runServe(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: rasputin serve\n\n"+
			"Serves the prepared and golden images and accepts captures, until\n"+
			"interrupted. Useful for triggering a reflash by hand.\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := newCluster(cfg)
	if err != nil {
		return err
	}
	srv, err := c.Serve()
	if err != nil {
		return err
	}
	defer srv.Close()

	var served int
	for _, img := range []cluster.Image{cluster.GoldenImage, cluster.VanillaImage} {
		if !img.Exists() {
			continue
		}
		if err := srv.Register(img.Name, img.Path); err != nil {
			return err
		}
		fmt.Printf("serving %s at %s\n", img.Path, srv.URLFor(img.Name))
		served++
	}
	if served == 0 {
		return fmt.Errorf("no images to serve: run `rasputin prepare` first")
	}
	fmt.Printf("\nto reflash a node by hand:\n"+
		"  ssh <node> 'echo %s | sudo tee %s && sudo reboot'\n\n"+
		"press ctrl-c to stop\n", srv.URLFor(cluster.GoldenImage.Name), cluster.FlagReflash)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("\nstopping")
	return nil
}
