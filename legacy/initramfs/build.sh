#!/bin/sh
# Build recovery.gz — the recovery initramfs for the rasputin Pi 3 cluster.
#
# Contents: static aarch64 busybox (Alpine's busybox-static) + static aarch64
# zstd (compiled from pinned source, since busybox lacks a zstd applet) + init.
#
# Runs the real work inside an arm64 Alpine container so that:
#   - it works from macOS or any Linux host with Docker
#   - the aarch64 binaries can be EXECUTED for verification, not just file(1)'d
# Downloads/builds are cached in initramfs/cache/ — delete it to force refetch.
set -eu

ZSTD_VER=1.5.6
ALPINE_IMG=alpine:3.20

cd "$(dirname "$0")"

# --- re-exec inside an arm64 Alpine container unless already there -----------
if [ ! -f /etc/alpine-release ]; then
    command -v docker >/dev/null 2>&1 \
        || { echo "error: docker required (or run on Alpine aarch64)" >&2; exit 1; }
    REPO=$(cd .. && pwd)
    exec docker run --rm --platform linux/arm64 \
        -v "$REPO":/work -w /work/initramfs "$ALPINE_IMG" sh ./build.sh
fi

[ "$(uname -m)" = aarch64 ] || { echo "error: container is not aarch64" >&2; exit 1; }
apk add -q file

# --- fetch static busybox ----------------------------------------------------
mkdir -p cache
if [ ! -x cache/busybox ]; then
    apk add -q busybox-static
    apk info busybox-static | head -1 > cache/busybox.version
    cp /bin/busybox.static cache/busybox
    chmod 755 cache/busybox
fi

# --- build static zstd (one-time, cached) ------------------------------------
if [ ! -x cache/zstd ]; then
    echo "building static zstd $ZSTD_VER from source (one-time, cached)..."
    apk add -q build-base
    wget -q -O /tmp/zstd.tar.gz \
        "https://github.com/facebook/zstd/releases/download/v$ZSTD_VER/zstd-$ZSTD_VER.tar.gz"
    tar -C /tmp -xzf /tmp/zstd.tar.gz
    make -C "/tmp/zstd-$ZSTD_VER/programs" -j"$(nproc)" zstd \
        LDFLAGS=-static HAVE_ZLIB=0 HAVE_LZMA=0 HAVE_LZ4=0 BACKTRACE=0 >/dev/null
    strip "/tmp/zstd-$ZSTD_VER/programs/zstd"
    cp "/tmp/zstd-$ZSTD_VER/programs/zstd" cache/zstd
    chmod 755 cache/zstd
fi

# --- verify: every binary must be static aarch64, and must actually run ------
check_binary() {
    out=$(file -b "$1")
    echo "  $1: $out"
    echo "$out" | grep -q "ARM aarch64" \
        || { echo "error: $1 is not aarch64" >&2; exit 1; }
    echo "$out" | grep -Eq "statically linked|static-pie linked" \
        || { echo "error: $1 is not static" >&2; exit 1; }
}
echo "verifying binaries:"
check_binary cache/busybox
check_binary cache/zstd

applets=$(cache/busybox 2>&1 || true)
for a in ash mount umount switch_root udhcpc wget dd sync reboot ip ifconfig \
         route mdev blkid head tail cat cut grep sed sleep usleep tr; do
    echo "$applets" | grep -qw "$a" \
        || { echo "error: busybox lacks required applet: $a" >&2; exit 1; }
done
cache/busybox wget --help 2>&1 | grep -q spider \
    || { echo "error: busybox wget lacks --spider (probe needs it)" >&2; exit 1; }
cache/zstd --version >/dev/null
echo "  applets + wget --spider + zstd --version: OK"

# --- assemble initramfs root -------------------------------------------------
. ../config/cluster.conf

rm -rf root
mkdir -p root/bin root/sbin root/usr/bin root/usr/sbin root/etc \
         root/dev root/proc root/sys root/tmp root/mnt/boot root/mnt/root
install -m 755 cache/busybox  root/bin/busybox
install -m 755 cache/zstd     root/bin/zstd
install -m 755 udhcpc.script  root/etc/udhcpc.script
sed "s|@DEFAULT_URL@|$DEFAULT_IMAGE_URL|" init > root/init
chmod 755 root/init
grep -q "@DEFAULT_URL@" root/init && { echo "error: URL substitution failed" >&2; exit 1; }

# /dev/console (c 5:1, mode 0600) so /init has a console before devtmpfs is
# mounted. mknod is blocked in unprivileged containers, so emit the newc cpio
# entry as raw bytes: magic, then ino mode uid gid nlink mtime filesize
# devmajor devminor rdevmajor rdevminor namesize check, then NUL-padded name.
console_cpio_entry() {
    printf '%s' 070701 00000001 00002180 00000000 00000000 00000001 \
                00000000 00000000 00000000 00000000 00000005 00000001 \
                0000000c 00000000
    printf 'dev/console\000\000\000'
}

# --- pack --------------------------------------------------------------------
{
    console_cpio_entry
    (cd root && find . | cpio -o -H newc)
} | gzip -9 > recovery.gz

echo
echo "recovery.gz built:"
ls -l recovery.gz
sha256sum recovery.gz
echo "contents:"
zcat recovery.gz | cpio -t
