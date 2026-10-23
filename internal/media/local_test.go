package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestLocal(t *testing.T) (*Local, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := NewLocal(dir)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return l, dir
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func tmpEntries(t *testing.T, dir string) int {
	t.Helper()
	es, err := os.ReadDir(filepath.Join(dir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}

func TestPutOpenDeleteRoundTrip(t *testing.T) {
	l, dir := newTestLocal(t)
	ctx := context.Background()
	data := []byte("a voice note, as recorded")

	key, n, err := l.Put(ctx, bytes.NewReader(data), 1<<20)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if key != sum(data) || n != int64(len(data)) {
		t.Fatalf("Put = %s, %d; want %s, %d", key, n, sum(data), len(data))
	}
	// Trestle's layout: blobs/<hh>/<sha256>.
	if _, err := os.Stat(filepath.Join(dir, "blobs", key[:2], key)); err != nil {
		t.Fatalf("blob is not at blobs/<hh>/<sha256>: %v", err)
	}
	if got := tmpEntries(t, dir); got != 0 {
		t.Fatalf("tmp/ holds %d entries after a committed put", got)
	}

	rc, size, err := l.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) || size != int64(len(data)) {
		t.Fatalf("Open returned %q (%d bytes)", got, size)
	}

	if err := l.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := l.Open(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after Delete = %v; want ErrNotFound", err)
	}
	if err := l.Delete(ctx, key); err != nil {
		t.Fatalf("a second Delete must be nil: %v", err)
	}
}

func TestPutIsIdempotentOnTheHash(t *testing.T) {
	l, dir := newTestLocal(t)
	data := []byte("the same bytes")
	k1, _, err := l.Put(context.Background(), bytes.NewReader(data), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	k2, _, err := l.Put(context.Background(), bytes.NewReader(data), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatalf("keys differ: %s vs %s", k1, k2)
	}
	if got := tmpEntries(t, dir); got != 0 {
		t.Fatalf("tmp/ holds %d entries after an idempotent put", got)
	}
}

func TestPutAtTheCapIsKeptAndOnePastIsRefusedWithNothingLeft(t *testing.T) {
	l, dir := newTestLocal(t)
	ctx := context.Background()
	if _, _, err := l.Put(ctx, bytes.NewReader(make([]byte, 100)), 100); err != nil {
		t.Fatalf("exactly max bytes must be accepted: %v", err)
	}
	over := make([]byte, 101)
	over[0] = 1
	_, _, err := l.Put(ctx, bytes.NewReader(over), 100)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("one byte past the cap = %v; want ErrTooLarge", err)
	}
	if _, _, err := l.Open(ctx, sum(over)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an over-cap upload left a blob behind: %v", err)
	}
	if got := tmpEntries(t, dir); got != 0 {
		t.Fatalf("tmp/ holds %d entries after an over-cap put", got)
	}
}

type failingReader struct{ sent bool }

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.sent {
		f.sent = true
		return copy(p, "partial"), nil
	}
	return 0, errors.New("connection reset")
}

func TestAnAbortedUploadLeavesNoPartialFile(t *testing.T) {
	l, dir := newTestLocal(t)
	if _, _, err := l.Put(context.Background(), &failingReader{}, 1<<20); err == nil {
		t.Fatal("Put succeeded on a reader that failed")
	}
	if got := tmpEntries(t, dir); got != 0 {
		t.Fatalf("tmp/ holds %d entries after an aborted put", got)
	}
	if _, _, err := l.Open(context.Background(), sum([]byte("partial"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an aborted upload committed its partial bytes: %v", err)
	}
}

func TestACancelledContextStopsThePut(t *testing.T) {
	l, dir := newTestLocal(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := l.Put(ctx, strings.NewReader("never stored"), 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put on a cancelled context = %v; want context.Canceled", err)
	}
	if got := tmpEntries(t, dir); got != 0 {
		t.Fatalf("tmp/ holds %d entries", got)
	}
}

func TestAKeyThatIsNotAKeyNamesNothing(t *testing.T) {
	l, dir := newTestLocal(t)
	// A file outside blobs/ that a traversal would reach.
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "../secret", "../../etc/passwd", "ABCDEF", strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("A", 64)} {
		if _, _, err := l.Open(context.Background(), bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%q) = %v; want ErrNotFound", bad, err)
		}
		if err := l.Delete(context.Background(), bad); err != nil {
			t.Errorf("Delete(%q) = %v; want nil", bad, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "secret")); err != nil {
		t.Fatalf("Delete reached outside blobs/: %v", err)
	}
}

func TestNewLocalSweepsTmpAndRefusesAnUnwritableDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tmp", "put-stale"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal(dir); err != nil {
		t.Fatal(err)
	}
	if got := tmpEntries(t, dir); got != 0 {
		t.Fatalf("tmp/ still holds %d stale entries after boot", got)
	}

	// A path under a regular file cannot be a directory.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal(filepath.Join(file, "media")); err == nil {
		t.Fatal("NewLocal accepted a directory that cannot exist")
	}
	if _, err := NewLocal(""); err == nil {
		t.Fatal("NewLocal accepted an empty directory")
	}
}
