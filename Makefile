# rasputin — Pi cluster reflash CLI.
#
# `build` produces the CLI, the aarch64 recovery initramfs (out/recovery.gz)
# and the man page (out/rasputin.1; `make install-man` puts it on the path).
# The initramfs is assembled by cmd/mkinitramfs, which shells out to `go build`
# for the agent, so the repo must be the working directory.

GO ?= go
OUT := out

MANDIR ?= /usr/local/share/man/man1

.PHONY: build man install-man test clean

build: man
	$(GO) build -o $(OUT)/rasputin ./cmd/rasputin
	$(GO) run ./cmd/rasputin prepare -initramfs-only

# The man page is rendered from cmd/rasputin/MANUAL.md by the binary itself.
man:
	mkdir -p $(OUT)
	$(GO) run ./cmd/rasputin manual -man > $(OUT)/rasputin.1

install-man: man
	install -d $(MANDIR)
	install -m 0644 $(OUT)/rasputin.1 $(MANDIR)/rasputin.1

test:
	$(GO) test ./...
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build ./...

clean:
	rm -rf $(OUT)
