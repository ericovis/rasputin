package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/ericovis/rasputin/internal/config"
)

// sudoPassword obtains the sudo password for a cluster configured with
// `ssh.sudo: password`.
//
// It is deliberately never read from rasputin.yaml: that file is committed
// to git, and a password in git is a password published. The environment
// variable covers unattended runs; the prompt covers everything else.
func sudoPassword(cfg *config.Config) (string, error) {
	if !cfg.SSH.NeedsSudoPassword() {
		return "", nil
	}
	if pw, ok := os.LookupEnv(config.SudoPasswordEnv); ok {
		if pw == "" {
			return "", fmt.Errorf("%s is set but empty", config.SudoPasswordEnv)
		}
		return pw, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf(
			"ssh.sudo is %q but there is no terminal to prompt on; set %s instead",
			config.SudoPassword, config.SudoPasswordEnv)
	}
	fmt.Fprintf(os.Stderr, "sudo password for %s on the cluster: ", strings.Join(cfg.SSH.Users, "/"))
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading the sudo password: %w", err)
	}
	pw := strings.TrimRight(string(raw), "\r\n")
	if pw == "" {
		return "", fmt.Errorf("no sudo password given")
	}
	return pw, nil
}
