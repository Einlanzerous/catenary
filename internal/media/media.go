// Package media is where uploaded bytes live. CANT-47 decided local disk behind
// this interface, in Trestle's layout, so that a later move into Trestle or R2
// is a copy of a directory tree and a new implementation of Store, not a change
// to anything that calls it.
//
// IT KNOWS NOTHING ABOUT WHAT A BLOB IS. Names, kinds and MIME types live in
// Postgres (the uploads table), never in a path; the key is the SHA-256 of the
// bytes as stored, and that is all this package is told.
package media

import (
	"context"
	"errors"
	"io"
	"regexp"
)

// Store is CANT-47's MediaStore.
type Store interface {
	// Put streams r into the store under max bytes, hashes what it read and
	// commits it, returning the key (the lowercase hex SHA-256 of the bytes)
	// and their count. Putting the same bytes twice is the same key and one
	// file. Nothing is committed when r errors, the context ends, or r holds
	// more than max bytes (ErrTooLarge).
	Put(ctx context.Context, r io.Reader, max int64) (key string, bytes int64, err error)

	// Open returns the blob and its size. ErrNotFound when there is none, and
	// for a key that is not a key at all.
	Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error)

	// Delete removes the blob. Idempotent: deleting what is not there is nil.
	Delete(ctx context.Context, key string) error
}

var (
	// ErrNotFound is Open's answer for a key with no blob.
	ErrNotFound = errors.New("media: not found")

	// ErrTooLarge is Put's answer for a reader with more than max bytes.
	ErrTooLarge = errors.New("media: over the byte cap")
)

var keyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidKey reports whether key is a SHA-256 in lowercase hex. Every
// implementation checks it before a key reaches a path, so a key from a URL can
// never name anything outside the store.
func ValidKey(key string) bool { return keyPattern.MatchString(key) }
