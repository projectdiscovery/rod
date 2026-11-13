package launcher

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeELF(t *testing.T, interp string) string {
	t.Helper()
	data := []byte(interp + "\x00")
	hdr := elf.Header64{
		Type:      uint16(elf.ET_EXEC),
		Machine:   uint16(elf.EM_X86_64),
		Version:   uint32(elf.EV_CURRENT),
		Phoff:     64,
		Ehsize:    64,
		Phentsize: 56,
		Phnum:     1,
		Shentsize: 64,
	}
	copy(hdr.Ident[:], elf.ELFMAG)
	hdr.Ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	hdr.Ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	hdr.Ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	prog := elf.Prog64{
		Type:   uint32(elf.PT_INTERP),
		Flags:  uint32(elf.PF_R),
		Off:    64 + 56,
		Filesz: uint64(len(data)),
		Memsz:  uint64(len(data)),
		Align:  1,
	}
	var buf bytes.Buffer
	err := binary.Write(&buf, binary.LittleEndian, hdr)
	if err != nil {
		t.Fatal(err)
	}
	err = binary.Write(&buf, binary.LittleEndian, prog)
	if err != nil {
		t.Fatal(err)
	}
	buf.Write(data)
	path := filepath.Join(t.TempDir(), "sh")
	err = os.WriteFile(path, buf.Bytes(), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestElfUsesMusl(t *testing.T) {
	if !elfUsesMusl(writeELF(t, "/lib/ld-musl-x86_64.so.1")) {
		t.Fatal("musl loader not detected")
	}
	if elfUsesMusl(writeELF(t, "/lib64/ld-linux-x86-64.so.2")) {
		t.Fatal("glibc loader detected as musl")
	}

	text := filepath.Join(t.TempDir(), "text")
	err := os.WriteFile(text, []byte("#!/bin/sh\n"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if elfUsesMusl(text) || elfUsesMusl(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("non-ELF file detected as musl")
	}
}

func TestGetOnMuslDoesNotDownload(t *testing.T) {
	old := hostUsesMusl
	hostUsesMusl = func() bool { return true }
	defer func() { hostUsesMusl = old }()

	b := NewBrowser()
	b.RootDir = t.TempDir()
	bin, err := b.Get()

	if found, has := LookPath(); has {
		if err != nil || bin != found {
			t.Fatalf("got %q, %v, want the system browser %q", bin, err, found)
		}
	} else if !errors.Is(err, ErrMuslNoBrowser) {
		t.Fatalf("got %q, %v, want ErrMuslNoBrowser", bin, err)
	}

	entries, err := os.ReadDir(b.RootDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("downloaded into %s", b.RootDir)
	}
}
