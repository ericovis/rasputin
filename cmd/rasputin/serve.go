package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// runServe runs the HTTP server on its own, for debugging a node that is
// downloading (or refusing to download) an image.
func runServe(cfg *config.Config, out *output, args []string) error {
	fs := out.flagSet("serve")
	fs.Usage = func() {
		out.printfErr("usage: rasputin serve [flags]\n\n" +
			"Serves the prepared and golden images and accepts captures, until\n" +
			"interrupted. Useful for triggering a reflash by hand.\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := newCluster(cfg, out)
	if err != nil {
		return err
	}
	srv, err := c.Serve()
	if err != nil {
		return err
	}
	defer srv.Close()

	var images []imageJSON
	for _, img := range []cluster.Image{cluster.GoldenImage, cluster.VanillaImage} {
		if !img.Exists() {
			continue
		}
		if err := srv.Register(img.Name, img.Path); err != nil {
			return err
		}
		out.printf("serving %s at %s\n", img.Path, srv.URLFor(img.Name))
		images = append(images, imageJSON{Name: img.Name, Path: img.Path, URL: srv.URLFor(img.Name)})
	}
	if len(images) == 0 {
		return fmt.Errorf("no images to serve: run `rasputin prepare` first")
	}
	goldenURL := srv.URLFor(cluster.GoldenImage.Name)
	trigger := fmt.Sprintf("ssh <node> 'echo %s | sudo tee %s && sudo reboot'", goldenURL, cluster.FlagReflash)
	out.printf("\nto reflash a node by hand:\n  %s\n\npress ctrl-c to stop\n", trigger)

	// In JSON mode the caller needs the URLs now, not when the server
	// stops, so the first line on the stream says what is being served.
	serving := struct {
		Type        string      `json:"type"`
		Images      []imageJSON `json:"images"`
		ReflashFlag string      `json:"reflash_flag"`
		Trigger     string      `json:"trigger"`
	}{"serving", images, cluster.FlagReflash, trigger}
	if out.json {
		out.emit(serving)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	out.printf("\nstopping\n")
	return out.result("serve", struct {
		Images []imageJSON `json:"images"`
	}{images}, nil)
}
