package launcher

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-rod/rod/lib/utils"
)

type zipEntry struct {
	name, body string
	mode       os.FileMode
}

func buildZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	buf := bytes.NewBuffer(nil)
	z := zip.NewWriter(buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h.SetMode(mode)
		w, err := z.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(e.body))
	}
	err := z.Close()
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func serveZip(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestFetchArchiveExtracts(t *testing.T) {
	data := buildZip(t,
		zipEntry{name: "top/bin/chrome", body: "exe", mode: 0o755},
		zipEntry{name: "top/lib/link", body: "../bin/chrome", mode: os.ModeSymlink | 0o777},
	)
	s := serveZip(t, data)
	dir := filepath.Join(t.TempDir(), "browser")

	sum := sha256.Sum256(data)
	_, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, hex.EncodeToString(sum[:]), []string{s.URL + "/a.zip"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "bin", "chrome"))
	if err != nil || string(b) != "exe" {
		t.Fatalf("bin: %q %v", b, err)
	}
	info, err := os.Stat(filepath.Join(dir, "bin", "chrome"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("mode: %v %v", info, err)
	}
	l, err := os.Readlink(filepath.Join(dir, "lib", "link"))
	if err != nil || l != filepath.FromSlash("../bin/chrome") {
		t.Fatalf("link: %q %v", l, err)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), ".rod-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestFetchArchiveChecksum(t *testing.T) {
	s := serveZip(t, buildZip(t, zipEntry{name: "a/b", body: "x"}))
	dir := filepath.Join(t.TempDir(), "browser")
	_, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "00", []string{s.URL})
	if err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	_, err = os.Stat(dir)
	if !os.IsNotExist(err) {
		t.Fatal("dir created on checksum failure")
	}
}

func TestFetchArchiveRejectsEscapes(t *testing.T) {
	cases := map[string][]zipEntry{
		"dotdot":       {{name: "../evil", body: "x"}},
		"nested":       {{name: "a/../../evil", body: "x"}},
		"abs":          {{name: "/tmp/evil", body: "x"}},
		"symlink-out":  {{name: "a/l", body: "../../outside", mode: os.ModeSymlink | 0o777}},
		"symlink-abs":  {{name: "a/l", body: "/etc/passwd", mode: os.ModeSymlink | 0o777}},
		"backslash-up": {{name: `a\..\..\evil`, body: "x"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			s := serveZip(t, buildZip(t, entries...))
			root := t.TempDir()
			dir := filepath.Join(root, "browser")
			_, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", []string{s.URL})
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Fatalf("expected unsafe archive, got %v", err)
			}
			_, err = os.Stat(filepath.Join(root, "evil"))
			if !os.IsNotExist(err) {
				t.Fatal("escaped file written")
			}
		})
	}
}

func TestFetchArchiveFallsBack(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer bad.Close()
	good := serveZip(t, buildZip(t, zipEntry{name: "a/b", body: "x"}))
	dir := filepath.Join(t.TempDir(), "browser")
	_, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", []string{bad.URL, good.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", []string{bad.URL})
	if err == nil {
		t.Fatal("404 accepted")
	}
}
