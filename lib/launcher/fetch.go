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
	"sync/atomic"
	"time"

	"github.com/go-rod/rod/lib/utils"
)

// MaxDownloadSize caps a browser archive and its extracted content.
var MaxDownloadSize int64 = 2 << 30

// DownloadIdleTimeout aborts a browser download that receives no bytes for
// this long. Without it a stalled CDN connection blocks the launch forever.
var DownloadIdleTimeout = 60 * time.Second

// ErrUnsafeArchive is returned when an archive entry would escape the target directory.
var ErrUnsafeArchive = errors.New("archive entry escapes target directory")

// fetchArchive downloads the first url that answers 200, verifies sum when it
// is not empty, and extracts it into dir. dir is replaced atomically.
// The returned sum is the SHA-256 of the archive.
func fetchArchive(
	ctx context.Context, client *http.Client, logger utils.Logger, dir, sum string, urls []string,
) (string, error) {
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

	err = os.MkdirAll(filepath.Dir(dir), 0o755)
	if err != nil {
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

	var stalled atomic.Bool
	idle := time.AfterFunc(DownloadIdleTimeout, func() {
		stalled.Store(true)
		_ = res.Body.Close()
	})
	defer idle.Stop()

	h := sha256.New()
	body := &idleBody{r: res.Body, timer: idle, timeout: DownloadIdleTimeout}
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, MaxDownloadSize+1))
	if err != nil {
		if stalled.Load() {
			return "", fmt.Errorf("browser download from %s stalled for %s", u, DownloadIdleTimeout)
		}
		return "", err
	}
	if n > MaxDownloadSize {
		return "", fmt.Errorf("browser archive larger than %d bytes", MaxDownloadSize)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if sum != "" && !strings.EqualFold(got, sum) {
		return "", fmt.Errorf("browser archive sha256 mismatch for %s", u)
	}

	err = installArchive(tmp, n, dir)
	if err != nil {
		return "", err
	}
	logger.Println("Downloaded:", dir)
	return got, nil
}

// installArchive extracts the zip in tmp into a staging dir and moves its
// single top-level directory to dir, replacing whatever was there.
func installArchive(tmp *os.File, n int64, dir string) error {
	staging, err := os.MkdirTemp(filepath.Dir(dir), ".rod-extract-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	err = unzipSafe(tmp, n, staging)
	if err != nil {
		return err
	}
	root, err := stripSingleDir(staging)
	if err != nil {
		return err
	}

	_ = os.RemoveAll(dir)
	return os.Rename(root, dir)
}

// idleBody re-arms the idle timer every time bytes arrive.
type idleBody struct {
	r       io.Reader
	timer   *time.Timer
	timeout time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.timer.Reset(b.timeout)
	}
	return n, err
}

// fetchResult is the outcome of one probe.
type fetchResult struct {
	res    *http.Response
	u      string
	err    error
	cancel context.CancelFunc
}

// fastest returns the first successful response and cancels the others.
func fastest(ctx context.Context, client *http.Client, urls []string) (*http.Response, string, error) {
	ch := make(chan fetchResult, len(urls))
	for _, u := range urls {
		c, cancel := context.WithCancel(ctx)
		go probe(c, client, u, cancel, ch)
	}

	var winner *fetchResult
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
		go drain(ch, remaining)
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

// probe requests u and sends the outcome on ch. A non-200 answer is an error.
func probe(ctx context.Context, client *http.Client, u string, cancel context.CancelFunc, ch chan<- fetchResult) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		ch <- fetchResult{u: u, err: err, cancel: cancel}
		return
	}
	res, err := client.Do(req)
	if err == nil && res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		err = fmt.Errorf("%s: %s", u, res.Status)
		res = nil
	}
	ch <- fetchResult{res: res, u: u, err: err, cancel: cancel}
}

// drain closes the remaining probe results and cancels them.
func drain(ch <-chan fetchResult, remaining int) {
	for range remaining {
		l := <-ch
		if l.res != nil {
			_ = l.res.Body.Close()
		}
		l.cancel()
	}
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
			err := os.MkdirAll(p, 0o755)
			if err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			err := extractSymlink(f, to, p)
			if err != nil {
				return err
			}
		default:
			err := extractFile(f, p, mode.Perm())
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func extractFile(f *zip.File, p string, perm os.FileMode) error {
	err := os.MkdirAll(filepath.Dir(p), 0o755)
	if err != nil {
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
	err = os.MkdirAll(filepath.Dir(p), 0o755)
	if err != nil {
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
