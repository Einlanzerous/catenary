// Package webui is the web client as the binary carries it (CANT-241, the
// build row of CANT-231's plan).
//
// THE EMBED NAMES `static`, NOT `static/dist`, AND THAT IS RULING 0's PICK. A
// `go:embed` pattern that matches nothing is a compile error, and Vite's output
// is never committed, so a directive over `static/dist` would make every `go
// build`, `go vet` and `go test` on a fresh clone depend on `npm`. The committed
// `static/PLACEHOLDER` keeps the directive matching on every checkout; whether a
// client was built is decided at run time, by whether `static/dist/index.html`
// is in the embedded tree. A binary without one serves no client and says so at
// boot (cmd/catenary's setup).
//
// Vite writes the client into `static/dist/` (web/vite.config.ts's outDir). The
// directory is ignored by .gitignore's unanchored `dist/` and excluded from the
// image's build context by .dockerignore, so the Dockerfile's Node stage is the
// image's only source of it.
package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
)

//go:embed all:static
var embedded embed.FS

// distDir is where Vite writes the client inside the embedded tree.
const distDir = "static/dist"

// Index is the bundle's HTML entry, served at `/`.
const Index = "index.html"

// contentTypes is the WHOLE table, one entry per extension Vite emits today.
//
// A FIXED TABLE RATHER THAN THE HOST'S MIME DATABASE. The standard library's
// lookup consults /etc/mime.types and its kin ahead of its own built-in table,
// so the same code can answer differently on a developer's machine, a CI runner
// and the Alpine image — and a browser refuses a module script served under a
// type that is not JavaScript. A file with any other extension fails Load, so a
// new asset class (an image, a font, a source map) is a deliberate one-line
// entry here in the change that introduces it.
var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
}

// ContentType reports the table's type for a file name's extension.
func ContentType(name string) (string, bool) {
	ct, ok := contentTypes[path.Ext(name)]
	return ct, ok
}

// literalPath is what a file name may contain and still be registered as a
// literal mux pattern. Anything else — a brace, a space, a percent sign — could
// read as a wildcard or need escaping, and fails Load rather than registering
// something other than the file.
var literalPath = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// File is one file of the bundle, read once at load.
type File struct {
	// Path is the file's path inside the bundle, slash-separated and with no
	// leading slash: `index.html`, `favicon.svg`, `assets/index-AbCd1234.js`.
	Path string

	Body        []byte
	ContentType string

	// ETag is a quoted strong validator: the SHA-256 of Body, computed once.
	ETag string
}

// Bundle is a loaded client: every file's bytes, content type and ETag.
type Bundle struct {
	files []File
}

// Files returns the bundle's files in path order. The slice is the caller's.
func (b *Bundle) Files() []File {
	return append([]File(nil), b.files...)
}

// Len is the number of files in the bundle.
func (b *Bundle) Len() int { return len(b.files) }

// Load reads a client from fsys, whose root is Vite's output directory.
//
// It refuses a tree with no index.html, a file whose extension is not in the
// content-type table, and a file name that cannot be a literal route pattern —
// each with an error naming the file. It takes any fs.FS so a test can hand it
// an in-memory fixture and exercise every route on a checkout that never ran
// `npm`.
func Load(fsys fs.FS) (*Bundle, error) {
	var files []File
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !literalPath.MatchString(p) {
			return fmt.Errorf("web client file %q cannot be served from a literal route pattern", p)
		}
		ct, ok := ContentType(p)
		if !ok {
			return fmt.Errorf("web client file %q has extension %q, which is not in internal/webui's content-type table", p, path.Ext(p))
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("web client file %q: %w", p, err)
		}
		sum := sha256.Sum256(body)
		files = append(files, File{
			Path:        p,
			Body:        body,
			ContentType: ct,
			ETag:        `"` + hex.EncodeToString(sum[:]) + `"`,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		if f.Path == Index {
			return &Bundle{files: files}, nil
		}
	}
	return nil, errors.New("web client has no " + Index)
}

// Embedded returns the client compiled into this binary, or nil when the binary
// was built without one — which is every binary built from a checkout that
// never ran `vite build`, and not an error. An error means a client IS embedded
// and cannot be served, which is a defect in the build that produced it.
func Embedded() (*Bundle, error) {
	if _, err := fs.Stat(embedded, path.Join(distDir, Index)); err != nil {
		return nil, nil
	}
	sub, err := fs.Sub(embedded, distDir)
	if err != nil {
		return nil, err
	}
	return Load(sub)
}
