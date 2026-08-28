# rasputin — Pi cluster reflash CLI.
#
# `build` produces the CLI plus the aarch64 recovery initramfs (out/recovery.gz).
# The initramfs is assembled by cmd/mkinitramfs, which shells out to `go build`
# for the agent, so the repo must be the working directory.

GO ?= go
OUT := out

.PHONY: build test clean

build:
	$(GO) build -o $(OUT)/rasputin ./cmd/rasputin
	$(GO) run ./cmd/mkinitramfs -c rasputin.yaml -o $(OUT)/recovery.gz

test:
	$(GO) test ./...
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build ./...

clean:
	rm -rf $(OUT)
