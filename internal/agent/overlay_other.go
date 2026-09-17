//go:build !linux

package agent

// The overlay is assembled with mount(2) and init_module(2), so off Linux
// these fail loudly rather than pretending. The decision logic around them —
// which module image to read, how to empty the writable layer — is portable
// and lives in overlay.go, where the build host's tests exercise it.

func WithUpperRW(func(string) error) error { return unsupported("mounting the writable layer") }

func initModule([]byte) error { return unsupported("loading a kernel module") }
