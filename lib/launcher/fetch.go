package launcher

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-rod/rod/lib/utils"
)

// MaxDownloadSize caps a browser archive and its extracted content.
var MaxDownloadSize int64 = 2 << 30

// ErrUnsafeArchive is returned when an archive entry would escape the target directory.
var ErrUnsafeArchive = errors.New("archive entry escapes target directory")

// fetchArchive downloads the first url that answers 200, verifies sum when it
// is not empty, and extracts it into dir. dir is replaced atomically.
// The returned sum is the SHA-256 of the archive.
func fetchArchive(ctx context.Context, client *http.Client, logger utils.Logger, dir, sum string, urls []string) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if len(urls) == 0 {
		return "", errors.New("no download urls")
	}

	res, u, err := fastest(ctx, client, urls)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = res.Body.Close()
		// HTTP/2 keeps a read goroutine after the body is closed. The leak
		// check treats that as a live goroutine.
		client.CloseIdleConnections()
	}()
	logger.Println("Download:", u)

	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dir), ".rod-download-*.zip")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(res.Body, MaxDownloadSize+1))
	if err != nil {
		return "", err
	}
	if n > MaxDownloadSize {
		return "", fmt.Errorf("browser archive larger than %d bytes", MaxDownloadSize)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if sum != "" && !strings.EqualFold(got, sum) {
		return "", fmt.Errorf("browser archive sha256 mismatch for %s", u)
	}

	staging, err := os.MkdirTemp(filepath.Dir(dir), ".rod-extract-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := unzipSafe(tmp, n, staging); err != nil {
		return "", err
	}
	root, err := stripSingleDir(staging)
	if err != nil {
		return "", err
	}

	_ = os.RemoveAll(dir)
	if err := os.Rename(root, dir); err != nil {
		return "", err
	}
	logger.Println("Downloaded:", dir)
	return got, nil
}

// fastest returns the first successful response and cancels the others.
func fastest(ctx context.Context, client *http.Client, urls []string) (*http.Response, string, error) {
	type result struct {
		res    *http.Response
		u      string
		err    error
		cancel context.CancelFunc
	}
	ch := make(chan result, len(urls))
	for _, u := range urls {
		u := u
		c, cancel := context.WithCancel(ctx)
		go func() {
			req, err := http.NewRequestWithContext(c, http.MethodGet, u, nil)
			if err != nil {
				ch <- result{u: u, err: err, cancel: cancel}
				return
			}
			res, err := client.Do(req)
			if err == nil && res.StatusCode != http.StatusOK {
				_ = res.Body.Close()
				err = fmt.Errorf("%s: %s", u, res.Status)
				res = nil
			}
			ch <- result{res: res, u: u, err: err, cancel: cancel}
		}()
	}

	var winner *result
	var errs []error
	for range urls {
		r := <-ch
		if r.err != nil || winner != nil {
			if r.res != nil {
				_ = r.res.Body.Close()
			}
			r.cancel()
			if r.err != nil {
				errs = append(errs, r.err)
			}
			continue
		}
		rr := r
		winner = &rr
		// Keep draining in the background so the losers get cancelled.
		remaining := len(urls) - 1 - len(errs)
		go func() {
			for i := 0; i < remaining; i++ {
				l := <-ch
				if l.res != nil {
					_ = l.res.Body.Close()
				}
				l.cancel()
			}
		}()
		break
	}
	if winner == nil {
		return nil, "", errors.Join(errs...)
	}
	body := winner.res.Body
	cancel := winner.cancel
	winner.res.Body = &cancelBody{ReadCloser: body, cancel: cancel}
	return winner.res, winner.u, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func unzipSafe(r io.ReaderAt, size int64, to string) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return err
	}

	var total uint64
	for _, f := range zr.File {
		total += f.UncompressedSize64
		if total > uint64(MaxDownloadSize) {
			return fmt.Errorf("extracted archive larger than %d bytes", MaxDownloadSize)
		}
	}

	for _, f := range zr.File {
		p, err := safeJoin(to, f.Name)
		if err != nil {
			return err
		}
		mode := f.Mode()

		switch {
		case f.FileInfo().IsDir():
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			if err := extractSymlink(f, to, p); err != nil {
				return err
			}
		default:
			if err := extractFile(f, p, mode.Perm()); err != nil {
				return err
			}
		}
	}
	return nil
}

func extractFile(f *zip.File, p string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm|0o200)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, io.LimitReader(src, int64(f.UncompressedSize64)+1))
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return err
}

func extractSymlink(f *zip.File, root, p string) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	target, err := io.ReadAll(io.LimitReader(src, 4096))
	_ = src.Close()
	if err != nil {
		return err
	}
	link := filepath.FromSlash(string(target))
	if filepath.IsAbs(link) {
		return fmt.Errorf("%w: %s -> %s", ErrUnsafeArchive, f.Name, link)
	}
	resolved := filepath.Join(filepath.Dir(p), link)
	if !within(root, resolved) {
		return fmt.Errorf("%w: %s -> %s", ErrUnsafeArchive, f.Name, link)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.Symlink(link, p)
}

func safeJoin(root, name string) (string, error) {
	name = filepath.FromSlash(strings.ReplaceAll(name, `\`, "/"))
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("%w: %s", ErrUnsafeArchive, name)
	}
	p := filepath.Join(root, name)
	if !within(root, p) {
		return "", fmt.Errorf("%w: %s", ErrUnsafeArchive, name)
	}
	return p, nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// stripSingleDir returns the only child directory of dir when there is
// exactly one entry, otherwise dir itself.
func stripSingleDir(dir string) (string, error) {
	list, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(list) == 1 && list[0].IsDir() {
		return filepath.Join(dir, list[0].Name()), nil
	}
	return dir, nil
}
