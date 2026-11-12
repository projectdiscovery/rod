package launcher

import (
	"debug/elf"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
)

// ErrMuslNoBrowser is returned by [Browser.Get] on a musl host, such as
// Alpine, when no browser is installed. Chrome for Testing and the Chromium
// snapshots are glibc builds and cannot run there.
var ErrMuslNoBrowser = errors.New("launcher: downloadable browsers need glibc and this host uses musl, install Chromium (apk add chromium) or set Launcher.Bin")

// hostUsesMusl reports whether this is a Linux host whose libc is musl.
var hostUsesMusl = sync.OnceValue(func() bool {
	return runtime.GOOS == "linux" && elfUsesMusl("/bin/sh")
})

// elfUsesMusl reports whether the ELF file at path is linked against the musl
// dynamic loader.
func elfUsesMusl(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		b, err := io.ReadAll(p.Open())
		return err == nil && strings.Contains(string(b), "ld-musl")
	}
	return false
}
