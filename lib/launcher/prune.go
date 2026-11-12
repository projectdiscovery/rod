package launcher

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PruneOlderBrowsers removes cached browser directories under root that are
// the same product as keep and an older version. keep is the directory of
// the version to retain. A directory whose version is not older, or whose
// name is not a downloaded browser, is left in place. The SHA-256 pin beside
// a removed directory is removed with it.
func PruneOlderBrowsers(root, keep string) (int, error) {
	prefix, keepVer, ok := browserVersion(filepath.Base(keep))
	if !ok {
		return 0, nil
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	keepName := filepath.Base(keep)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keepName {
			continue
		}
		p, ver, ok := browserVersion(e.Name())
		if !ok || p != prefix {
			continue
		}
		cmp, ok := compareVersion(ver, keepVer)
		if !ok || cmp >= 0 {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if os.RemoveAll(dir) != nil {
			continue
		}
		_ = os.Remove(dir + ".sha256")
		removed++
	}
	return removed, nil
}

func browserVersion(name string) (prefix, version string, ok bool) {
	for _, p := range []string{"chrome-headless-shell-", "chrome-", "chromium-"} {
		if rest, found := strings.CutPrefix(name, p); found && rest != "" {
			return p, rest, true
		}
	}
	return "", "", false
}

// compareVersion reports -1 when a is older than b. The second result is
// false when a component is not an integer.
func compareVersion(a, b string) (int, bool) {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(as) {
			v, err := strconv.Atoi(as[i])
			if err != nil {
				return 0, false
			}
			av = v
		}
		if i < len(bs) {
			v, err := strconv.Atoi(bs[i])
			if err != nil {
				return 0, false
			}
			bv = v
		}
		if av < bv {
			return -1, true
		}
		if av > bv {
			return 1, true
		}
	}
	return 0, true
}
