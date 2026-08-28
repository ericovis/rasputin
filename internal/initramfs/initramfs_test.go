package initramfs

import (
	"bytes"
	"compress/gzip"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericovis/rasputin/internal/cpio"
)

func TestBuildProducesABootableArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiling the agent is slow")
	}
	out := filepath.Join(t.TempDir(), "recovery.gz")
	res, err := Build(Options{
		RepoDir:    "../..",
		OutPath:    out,
		DefaultURL: "http://192.168.0.228:8080/golden.img.zst",
		Version:    "test",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.CompressedSize <= 0 || res.AgentSize <= 0 {
		t.Fatalf("Result = %+v", res)
	}

	entries := readArchive(t, out)
	console := cpio.Find(entries, "dev/console")
	if console == nil {
		t.Fatal("dev/console missing from the archive")
	}
	if !console.IsCharDev || console.Major != 5 || console.Minor != 1 || console.Mode != 0o600 {
		t.Errorf("dev/console = %+v, want char 5:1 mode 0600", *console)
	}
	if d := cpio.Find(entries, "dev"); d == nil || !d.IsDir {
		t.Error("dev directory missing from the archive")
	}

	init := cpio.Find(entries, "init")
	if init == nil {
		t.Fatal("init missing from the archive")
	}
	if init.Mode != 0o755 {
		t.Errorf("init mode = %o, want 755", init.Mode)
	}
	if err := VerifyAgent(init.Data); err != nil {
		t.Errorf("packed init: %v", err)
	}
	if int64(len(init.Data)) != res.AgentSize {
		t.Errorf("packed init is %d bytes, Result says %d", len(init.Data), res.AgentSize)
	}
	// The baked-in default URL must actually reach the binary, or a manual
	// `touch reflash` on a node would have nowhere to download from.
	if !bytes.Contains(init.Data, []byte("192.168.0.228:8080")) {
		t.Error("the -X main.defaultURL ldflag did not make it into the agent")
	}
}

func readArchive(t *testing.T, path string) []cpio.Entry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip %s: %v", path, err)
	}
	defer gz.Close()
	entries, err := cpio.Read(gz)
	if err != nil {
		t.Fatalf("cpio parse: %v", err)
	}
	return entries
}

func TestPackLayout(t *testing.T) {
	archive, err := Pack([]byte("not-an-elf"))
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	entries, err := cpio.Read(gz)
	if err != nil {
		t.Fatalf("cpio parse: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	if got, want := strings.Join(names, ","), "dev,dev/console,init"; got != want {
		t.Errorf("archive contains %q, want %q", got, want)
	}
}

func TestVerifyAgentRejections(t *testing.T) {
	if err := VerifyAgent([]byte("definitely not ELF")); err == nil {
		t.Error("non-ELF accepted")
	}
	if err := VerifyAgent(syntheticELF(elf.EM_X86_64, false)); err == nil {
		t.Error("x86-64 binary accepted, want a machine mismatch error")
	}
	if err := VerifyAgent(syntheticELF(elf.EM_AARCH64, true)); err == nil {
		t.Error("dynamically linked binary accepted, want a PT_INTERP error")
	}
	if err := VerifyAgent(syntheticELF(elf.EM_AARCH64, false)); err != nil {
		t.Errorf("static aarch64 binary rejected: %v", err)
	}
}

// syntheticELF builds the smallest little-endian 64-bit ELF that debug/elf
// will parse: a header, and optionally one PT_INTERP program header standing
// in for a dynamic loader.
func syntheticELF(machine elf.Machine, dynamic bool) []byte {
	const ehsize, phentsize = 64, 56
	buf := make([]byte, ehsize)
	copy(buf, []byte{0x7f, 'E', 'L', 'F'})
	buf[4] = byte(elf.ELFCLASS64)
	buf[5] = byte(elf.ELFDATA2LSB)
	buf[6] = byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(buf[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(buf[18:], uint16(machine))
	binary.LittleEndian.PutUint32(buf[20:], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(buf[52:], ehsize)
	binary.LittleEndian.PutUint16(buf[54:], phentsize)
	if dynamic {
		binary.LittleEndian.PutUint64(buf[32:], ehsize) // e_phoff
		binary.LittleEndian.PutUint16(buf[56:], 1)      // e_phnum
		ph := make([]byte, phentsize)
		binary.LittleEndian.PutUint32(ph[0:], uint32(elf.PT_INTERP))
		buf = append(buf, ph...)
	}
	return buf
}

func TestBuildRequiresOutPath(t *testing.T) {
	if _, err := Build(Options{}); err == nil {
		t.Error("Build with no OutPath accepted")
	}
}
