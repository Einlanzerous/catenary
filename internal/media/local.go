package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Local is the first Store: files under one directory, laid out as
// blobs/<hh>/<sha256>, which is Trestle's layout (docs/decisions/serv-134-trestle-v1.md
// in its repository).
//
// WRITES ARE TEMP-FILE-THEN-RENAME, in tmp/ beside blobs/ on the same
// filesystem, so a reader never sees a half-written blob and a crash leaves
// only a stray temp file. NewLocal sweeps tmp/ at boot, which is safe because a
// temp file belongs to a process that is no longer running.
type Local struct {
	dir string
}

// NewLocal prepares dir and returns a store rooted at it. It creates
// blobs/ and tmp/, empties tmp/, and proves the directory is writable by
// creating and removing a file, so a read-only volume fails at boot naming the
// directory and not at the first photo.
func NewLocal(dir string) (*Local, error) {
	if dir == "" {
		return nil, errors.New("media: no directory")
	}
	for _, sub := range []string{"blobs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, fmt.Errorf("media: prepare %s: %w", dir, err)
		}
	}
	tmp := filepath.Join(dir, "tmp")
	stale, err := os.ReadDir(tmp)
	if err != nil {
		return nil, fmt.Errorf("media: read %s: %w", tmp, err)
	}
	for _, e := range stale {
		if err := os.RemoveAll(filepath.Join(tmp, e.Name())); err != nil {
			return nil, fmt.Errorf("media: sweep %s: %w", tmp, err)
		}
	}
	probe, err := os.CreateTemp(tmp, "probe-*")
	if err != nil {
		return nil, fmt.Errorf("media: %s is not writable: %w", dir, err)
	}
	probe.Close()
	if err := os.Remove(probe.Name()); err != nil {
		return nil, fmt.Errorf("media: %s is not writable: %w", dir, err)
	}
	return &Local{dir: dir}, nil
}

func (l *Local) path(key string) string {
	return filepath.Join(l.dir, "blobs", key[:2], key)
}

// ctxReader fails reads once the context has ended, so a client that goes away
// mid-upload stops the copy rather than finishing it into a file nobody wants.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Put implements Store.
func (l *Local) Put(ctx context.Context, r io.Reader, max int64) (string, int64, error) {
	f, err := os.CreateTemp(filepath.Join(l.dir, "tmp"), "put-*")
	if err != nil {
		return "", 0, fmt.Errorf("media: temp file: %w", err)
	}
	committed := false
	defer func() {
		f.Close()
		if !committed {
			os.Remove(f.Name())
		}
	}()

	// One byte past the cap is what distinguishes "exactly max" from "more".
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(ctxReader{ctx, r}, max+1))
	if err != nil {
		return "", 0, fmt.Errorf("media: read upload: %w", err)
	}
	if n > max {
		return "", 0, ErrTooLarge
	}
	if err := f.Sync(); err != nil {
		return "", 0, fmt.Errorf("media: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", 0, fmt.Errorf("media: close: %w", err)
	}

	key := hex.EncodeToString(h.Sum(nil))
	dst := l.path(key)
	if _, err := os.Stat(dst); err == nil {
		// Same bytes already stored: idempotent, and the temp file goes.
		return key, n, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", 0, fmt.Errorf("media: prepare blob dir: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o640); err != nil {
		return "", 0, fmt.Errorf("media: chmod: %w", err)
	}
	if err := os.Rename(f.Name(), dst); err != nil {
		return "", 0, fmt.Errorf("media: commit: %w", err)
	}
	committed = true
	return key, n, nil
}

// Open implements Store.
func (l *Local) Open(_ context.Context, key string) (io.ReadSeekCloser, int64, error) {
	if !ValidKey(key) {
		return nil, 0, ErrNotFound
	}
	f, err := os.Open(l.path(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("media: open: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("media: stat: %w", err)
	}
	return f, st.Size(), nil
}

// Delete implements Store.
func (l *Local) Delete(_ context.Context, key string) error {
	if !ValidKey(key) {
		return nil
	}
	if err := os.Remove(l.path(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("media: delete: %w", err)
	}
	return nil
}
