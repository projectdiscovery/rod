package launcher

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectdiscovery/rod/lib/utils"
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

func TestFetchArchiveReplacesExisting(t *testing.T) {
	data := buildZip(t, zipEntry{name: "top/bin/chrome", body: "new"})
	s := serveZip(t, data)
	dir := filepath.Join(t.TempDir(), "browser")

	// A previous install with a stale file that the new archive does not have.
	err := os.MkdirAll(filepath.Join(dir, "old"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "old", "stale"), []byte("x"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(data)
	_, err = fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, hex.EncodeToString(sum[:]), []string{s.URL + "/a.zip"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "bin", "chrome"))
	if err != nil || string(b) != "new" {
		t.Fatalf("bin: %q %v", b, err)
	}
	_, err = os.Stat(filepath.Join(dir, "old"))
	if !os.IsNotExist(err) {
		t.Fatalf("stale install not replaced: %v", err)
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

// A server that sends headers and a few bytes, then goes silent, must not hang
// the download forever.
func TestFetchArchiveStalledBody(t *testing.T) {
	old := DownloadIdleTimeout
	DownloadIdleTimeout = 300 * time.Millisecond
	defer func() { DownloadIdleTimeout = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("PK"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "browser")
	done := make(chan error, 1)
	go func() {
		_, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", []string{srv.URL})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stalled") {
			t.Fatalf("expected stall error, got %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("download hung on a stalled body")
	}
}

// silentHost accepts connections and never answers. The test cleanup closes
// the listener and every connection, so a request that still waits ends.
func silentHost(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	closed := false
	accepted := &atomic.Int32{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			mu.Lock()
			if closed {
				_ = c.Close()
			} else {
				conns = append(conns, c)
			}
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		closed = true
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return "http://" + l.Addr().String(), accepted
}

// A host that accepts the connection and sends no headers must not hang the
// download when the caller set no client.
func TestFetchArchiveSilentHost(t *testing.T) {
	old := DownloadHeaderTimeout
	DownloadHeaderTimeout = 300 * time.Millisecond
	defer func() { DownloadHeaderTimeout = old }()

	u, _ := silentHost(t)
	dir := filepath.Join(t.TempDir(), "browser")
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", []string{u + "/a.zip"})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no response headers") {
			t.Fatalf("expected a header timeout, got %v", err)
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("header timeout took %v", took)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("download hung on a host that sends no headers")
	}
}

// The header bound ends when the headers arrive. A body that takes longer
// than the bound still downloads with the default client.
func TestFetchArchiveSlowBodyAfterHeaders(t *testing.T) {
	old := DownloadHeaderTimeout
	DownloadHeaderTimeout = 50 * time.Millisecond
	defer func() { DownloadHeaderTimeout = old }()

	data := buildZip(t, zipEntry{name: "top/bin/chrome", body: "exe", mode: 0o755})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		half := len(data) / 2
		_, _ = w.Write(data[:half])
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write(data[half:])
	}))
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "browser")
	_, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", []string{srv.URL})
	if err != nil {
		t.Fatal(err)
	}
}

// Zero turns the header bound off.
func TestFetchArchiveHeaderTimeoutOff(t *testing.T) {
	old := DownloadHeaderTimeout
	DownloadHeaderTimeout = 0
	defer func() { DownloadHeaderTimeout = old }()

	data := buildZip(t, zipEntry{name: "top/bin/chrome", body: "exe", mode: 0o755})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "browser")
	_, err := fetchArchive(context.Background(), nil, utils.LoggerQuiet, dir, "", []string{srv.URL})
	if err != nil {
		t.Fatal(err)
	}
}

// The header timeout applies only to the default client. A client that the
// caller set is used as it is, so a slow header answer still works.
func TestFetchArchiveCallerClientNoHeaderTimeout(t *testing.T) {
	old := DownloadHeaderTimeout
	DownloadHeaderTimeout = time.Millisecond
	defer func() { DownloadHeaderTimeout = old }()

	data := buildZip(t, zipEntry{name: "top/bin/chrome", body: "exe", mode: 0o755})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	client := &http.Client{}
	defer client.CloseIdleConnections()
	dir := filepath.Join(t.TempDir(), "browser")
	_, err := fetchArchive(context.Background(), client, utils.LoggerQuiet, dir, "", []string{srv.URL})
	if err != nil {
		t.Fatal(err)
	}
}
