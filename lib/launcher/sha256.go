package launcher

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// CheckFileSHA256 reports whether path hashes to sum. sum is hex, with or
// without surrounding whitespace. Use this for a browser archive you downloaded
// yourself. The built-in downloader does not see the archive bytes, so it
// cannot apply the pin itself.
func CheckFileSHA256(path, sum string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	want := strings.ToLower(strings.TrimSpace(sum))
	if got != want {
		return fmt.Errorf("sha256 mismatch for %s: got %s", path, got)
	}
	return nil
}
