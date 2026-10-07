package webui

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func fixture() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                {Data: []byte(`<!doctype html><div id="app"></div><script type="module" src="/assets/index-AAAAAAAA.js"></script>`)},
		"favicon.svg":               {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)},
		"assets/index-AAAAAAAA.js":  {Data: []byte(`console.log(1)`)},
		"assets/index-BBBBBBBB.css": {Data: []byte(`body{}`)},
	}
}

// Content-type table (plan criterion 8): exactly four entries, and nothing else
// is guessed.
func TestTheContentTypeTableIsTheFourEntries(t *testing.T) {
	want := map[string]string{
		".html": "text/html; charset=utf-8",
		".js":   "text/javascript; charset=utf-8",
		".css":  "text/css; charset=utf-8",
		".svg":  "image/svg+xml",
	}
	if len(contentTypes) != len(want) {
		t.Errorf("the table has %d entries, want %d: %v", len(contentTypes), len(want), contentTypes)
	}
	for ext, ct := range want {
		if got, ok := ContentType("x" + ext); !ok || got != ct {
			t.Errorf("ContentType(x%s) = %q, %v; want %q", ext, got, ok, ct)
		}
	}

	b, err := Load(fixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range b.Files() {
		if f.ContentType != want[filepath.Ext(f.Path)] {
			t.Errorf("%s loaded as %q", f.Path, f.ContentType)
		}
	}
}

func TestAnExtensionOutsideTheTableFailsTheLoadByName(t *testing.T) {
	fsys := fixture()
	fsys["assets/x.woff2"] = &fstest.MapFile{Data: []byte("font")}
	_, err := Load(fsys)
	if err == nil {
		t.Fatal("a bundle carrying assets/x.woff2 loaded; it would be served under a guessed type")
	}
	if !strings.Contains(err.Error(), "assets/x.woff2") {
		t.Errorf("the error does not name the file: %v", err)
	}
}

func TestANameThatIsNotALiteralPatternFailsTheLoad(t *testing.T) {
	fsys := fixture()
	fsys["assets/{id}.js"] = &fstest.MapFile{Data: []byte("x")}
	_, err := Load(fsys)
	if err == nil || !strings.Contains(err.Error(), "assets/{id}.js") {
		t.Fatalf("a wildcard-shaped file name loaded, or the error does not name it: %v", err)
	}
}

func TestABundleWithNoDocumentFailsTheLoad(t *testing.T) {
	fsys := fixture()
	delete(fsys, "index.html")
	if _, err := Load(fsys); err == nil {
		t.Fatal("a bundle with no index.html loaded")
	}
}

func TestEveryFileCarriesAQuotedStrongETag(t *testing.T) {
	b, err := Load(fixture())
	if err != nil {
		t.Fatal(err)
	}
	strong := regexp.MustCompile(`^"[0-9a-f]{64}"$`)
	for _, f := range b.Files() {
		if !strong.MatchString(f.ETag) {
			t.Errorf("%s ETag = %s, want a quoted SHA-256", f.Path, f.ETag)
		}
	}
}

// No file in internal/webui or internal/api reaches the host's MIME database.
// Asserted on the IMPORT, which is stronger than a grep for the call: a source
// that never imports the package cannot call any of its lookups.
func TestNeitherPackageImportsTheHostMIMEDatabase(t *testing.T) {
	for _, dir := range []string{".", filepath.Join("..", "api")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			n++
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); p == "mime" {
					t.Errorf("%s imports %q; content types come from internal/webui's table", filepath.Join(dir, name), p)
				}
			}
		}
		if n == 0 {
			t.Errorf("read no non-test sources in %s; the assertion proved nothing", dir)
		}
	}
}

// Placeholder (criterion 2's tracked half, held here as well as in verify.sh):
// the embedded tree always holds the committed placeholder, so the directive
// matched on this checkout whatever else was built.
func TestThePlaceholderIsEmbedded(t *testing.T) {
	if _, err := embedded.ReadFile("static/PLACEHOLDER"); err != nil {
		t.Fatalf("static/PLACEHOLDER is not in the embedded tree: %v", err)
	}
}

// --- the real bundle (criterion 14) -------------------------------------------

// realBundle is the client this binary embeds. Gated like CATENARY_WEB_SMOKE:
// CATENARY_WEB_REQUIRED unset and no client skips; set and no client FAILS,
// because verify.sh sets it after building the client and a skip there would be
// a green line that checked nothing.
func realBundle(t *testing.T) *Bundle {
	t.Helper()
	b, err := Embedded()
	if err != nil {
		t.Fatalf("the embedded client does not load: %v", err)
	}
	if b == nil {
		if os.Getenv("CATENARY_WEB_REQUIRED") != "" {
			t.Fatal("CATENARY_WEB_REQUIRED is set and this binary embeds no client: run `npx --no-install vite build` in web/ first")
		}
		t.Skip("no web client embedded; `npx --no-install vite build` in web/ builds one (CATENARY_WEB_REQUIRED=1 makes this a failure)")
	}
	return b
}

func TestTheRealBundleIsServable(t *testing.T) {
	b := realBundle(t)

	byPath := map[string]File{}
	for _, f := range b.Files() {
		byPath[f.Path] = f
		if _, ok := ContentType(f.Path); !ok {
			t.Errorf("%s has an extension outside the table", f.Path)
		}
	}
	doc, ok := byPath[Index]
	if !ok {
		t.Fatal("the bundle has no index.html")
	}

	// Every asset the document names is a file in the bundle.
	refs := regexp.MustCompile(`(?:src|href)="/(assets/[^"]+)"`).FindAllStringSubmatch(string(doc.Body), -1)
	if len(refs) == 0 {
		t.Error("the document references no /assets/ file; it cannot be the built client")
	}
	for _, m := range refs {
		if _, ok := byPath[m[1]]; !ok {
			t.Errorf("the document references /%s, which is not in the bundle", m[1])
		}
	}

	// Everything under assets/ is content-hashed, because the cache policy is
	// keyed on the prefix: an unhashed file there would be cached for a year.
	hashed := regexp.MustCompile(`-[A-Za-z0-9_-]{8}\.[A-Za-z0-9]+$`)
	for p := range byPath {
		if strings.HasPrefix(p, "assets/") && !hashed.MatchString(p) {
			t.Errorf("%s is under assets/ and carries no hash-shaped name", p)
		}
	}

	// script-src 'self' is the directive that protects the credential, and it
	// holds only while the document runs no inline script.
	scripts := regexp.MustCompile(`(?is)<script\b[^>]*>(.*?)</script>`).FindAllStringSubmatch(string(doc.Body), -1)
	for _, m := range scripts {
		if strings.TrimSpace(m[1]) != "" {
			t.Errorf("the document carries an inline script body: %q", m[1])
		}
	}
	if m := regexp.MustCompile(`(?i)\son[a-z]+\s*=`).FindString(string(doc.Body)); m != "" {
		t.Errorf("the document carries an inline event handler attribute: %q", m)
	}
}
