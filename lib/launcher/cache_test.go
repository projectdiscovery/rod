package launcher

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectdiscovery/rod/lib/utils"
)

func TestPruneOlderBrowsers(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "chrome-headless-shell-157.0.1.0")
	old := filepath.Join(root, "chrome-headless-shell-152.0.1.0")
	newer := filepath.Join(root, "chrome-headless-shell-158.0.1.0")
	full := filepath.Join(root, "chrome-120.0.1.0")
	snap := filepath.Join(root, "chromium-100")
	for _, dir := range []string{keep, old, newer, full, snap} {
		err := os.Mkdir(dir, 0o755)
		if err != nil {
			t.Fatal(err)
		}
	}
	err := os.WriteFile(old+".sha256", []byte("abc\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	n, err := PruneOlderBrowsers(root, keep)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("removed %d", n)
	}
	_, err = os.Stat(old)
	if !os.IsNotExist(err) {
		t.Fatal("old version still present")
	}
	_, err = os.Stat(old + ".sha256")
	if !os.IsNotExist(err) {
		t.Fatal("old pin still present")
	}
	for _, dir := range []string{keep, newer, full, snap} {
		_, err := os.Stat(dir)
		if err != nil {
			t.Fatal(dir, err)
		}
	}
}

func TestRecordedPinRejectsReplacement(t *testing.T) {
	good := buildZip(t, zipEntry{name: "chrome-linux/chrome", body: "one"})
	bad := buildZip(t, zipEntry{name: "chrome-linux/chrome", body: "two"})
	payload := good
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer s.Close()

	b := NewBrowser()
	b.RootDir = t.TempDir()
	b.Revision = 7
	b.Hosts = []Host{func(int) string { return s.URL }}
	b.Logger = utils.LoggerQuiet
	err := b.Download()
	if err != nil {
		t.Fatal(err)
	}
	pin, err := os.ReadFile(b.Dir() + ".sha256")
	if err != nil || !strings.Contains(string(pin), "\n") {
		t.Fatalf("pin %q %v", pin, err)
	}

	payload = bad
	b.SHA256 = ""
	err = b.Download()
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("got %v", err)
	}
	body, err := os.ReadFile(filepath.Join(b.Dir(), "chrome"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "one" {
		t.Fatalf("install replaced with %q", body)
	}
}
