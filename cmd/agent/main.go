// Command agent is the rasputin recovery agent: a single static aarch64
// binary that serves as /init inside the recovery initramfs. Every boot of an
// adopted node runs it before anything else.
//
// It is PID 1. Exiting would panic the kernel, so every failure path here
// ends in a log loop or a reboot, never in os.Exit.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ericovis/rasputin/internal/agent"
	"github.com/ericovis/rasputin/internal/kmsg"
)

// defaultURL is baked in at build time with
// -ldflags "-X main.defaultURL=http://host:port/golden.img.zst" so that a
// bare `touch /boot/firmware/reflash && reboot` works with no CLI involved.
// Empty is legitimate: then a flag file must carry its own URL.
var defaultURL = ""

// version is stamped by the build for post-mortem log correlation.
var version = "dev"

func main() {
	// Refuse to run outside an initramfs unless explicitly testing: this
	// binary mounts filesystems and can overwrite a whole disk.
	if os.Getpid() != 1 && os.Getenv("RASPUTIN_AGENT_TEST") == "" {
		fmt.Fprintln(os.Stderr, "rasputin-agent: refusing to run: not PID 1 (set RASPUTIN_AGENT_TEST=1 to override)")
		os.Exit(1)
	}

	mountErr := agent.MountPseudoFS()
	log, kf, kerr := kmsg.Open()
	if kf != nil {
		defer kf.Close()
	}
	if mountErr != nil {
		log.Printf("WARNING: mounting pseudo filesystems: %v", mountErr)
	}
	if kerr != nil {
		log.Printf("WARNING: /dev/kmsg unavailable: %v", kerr)
	}
	log.Printf("recovery agent %s starting (pid %d)", version, os.Getpid())

	// A panic in PID 1 kills the machine; hold the node in a legible state
	// instead so an operator can read the console and power-cycle.
	defer func() {
		if r := recover(); r != nil {
			hold(log, fmt.Sprintf("PANIC: %v", r))
		}
	}()

	run(log)
}

func run(log *kmsg.Logger) {
	if err := agent.WaitForDisk(agent.DeviceWait); err != nil {
		hold(log, err.Error())
		return
	}

	files, err := agent.ReadFlags()
	if err != nil {
		// The boot partition is where every instruction comes from. Without
		// it there is nothing safe to do but wait for a human.
		hold(log, fmt.Sprintf("cannot read flags from %s: %v", agent.BootPart, err))
		return
	}
	mode, url := agent.SelectMode(files, defaultURL)
	log.Printf("mode=%s mac=%s url=%s", mode, agent.MAC(), url)

	switch mode {
	case agent.ModeNormal:
		if err := agent.SwitchRoot(log); err != nil {
			// Deliberately not a reboot: a reboot loop would hide the cause
			// and hammer the card. Hold and let the operator look.
			hold(log, fmt.Sprintf("switch_root failed: %v", err))
		}
	case agent.ModeError:
		hold(log, "a flag file is set but no image URL is available "+
			"(the flag file is empty and no default URL was baked in) — "+
			"the SD card has NOT been touched")
	default:
		runNetworkMode(log, mode, url)
	}
}

// runNetworkMode brings up the network and runs reflash, dryrun or capture.
// Each of those ends in a reboot; if one returns instead, something is wrong
// enough that holding is safer than looping.
func runNetworkMode(log *kmsg.Logger, mode agent.Mode, url string) {
	ctx := context.Background()
	if _, err := agent.NetworkUp(ctx, log); err != nil {
		hold(log, fmt.Sprintf("bringing up the network: %v", err))
		return
	}
	client := agent.NewClient(url, agent.MAC(), log)
	if err := agent.RunMode(ctx, mode, client, agent.SDCard(), agent.Sys()); err != nil {
		hold(log, fmt.Sprintf("mode %s: %v", mode, err))
		return
	}
	// RunMode only returns after a successful reboot call, which should not
	// return at all.
	hold(log, "reboot did not take effect")
}

// hold logs a fatal condition forever. PID 1 must not exit, and rebooting
// would only lose the reason.
func hold(log *kmsg.Logger, msg string) {
	for {
		log.Printf("FATAL: %s", msg)
		log.Print("holding here; the node needs manual recovery (power-cycle to retry)")
		time.Sleep(30 * time.Second)
	}
}
