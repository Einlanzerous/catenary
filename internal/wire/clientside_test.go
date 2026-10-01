package wire_test

// CANT-177 row 0 (CANT-180): the generated Go package's client side.
//
// The only hand-written file in this directory, and it is a test: everything it
// exercises is emitted by schema/codegen/generate.mjs, and the staleness guard
// holds generated.go to that. Two claims live here because no conformance
// vector can make them — the vectors fix what a decode returns, not what it
// reports, nor who may call it.
//
//   - THE REPORT. A client-side decode reports a value this schema version does
//     not define once per (enum, raw) per process, with TypeScript's text, and
//     the hook is safe to swap while another goroutine decodes.
//   - THE LINE BETWEEN THE SIDES. Only internal/client may name a wire.*AsClient
//     entrypoint. That is what makes the schema header's "on every decode the
//     server runs" true rather than hoped for: a handler that reached for the
//     client side would silently stop refusing an undefined client-open value.
//
// The report state is per process, so the once-per-pair assertion resets it
// first through export_test.go — otherwise `go test -count=2` would see the
// pair already reported and get no report at all.

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/magos/catenary/internal/wire"
)

// vector returns one case's input from the shared golden vectors.
func vector(t *testing.T, name string) json.RawMessage {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(moduleRoot(t), "schema", "vectors", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			Name string          `json:"name"`
			JSON json.RawMessage `json:"json"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Cases {
		if c.Name == name {
			return c.JSON
		}
	}
	t.Fatalf("no vector %q", name)
	return nil
}

func TestAClientSideDecodeReportsAnUndefinedValueOncePerProcess(t *testing.T) {
	var (
		mu      sync.Mutex
		reports []string
	)
	wire.SetOnUnknownWireValue(func(m string) {
		mu.Lock()
		reports = append(reports, m)
		mu.Unlock()
	})
	t.Cleanup(func() { wire.SetOnUnknownWireValue(nil) })
	wire.ResetUnknownWireValues()

	b := vector(t, "tolerate_unknown_error_code_keeps_retryable")

	// The strict side is unchanged: the server refuses what a client carries.
	if _, err := wire.DecodeServerFrame(b); err == nil {
		t.Fatal("DecodeServerFrame accepted an undefined error code; the server side must refuse it")
	}

	for i := 0; i < 2; i++ {
		f, err := wire.DecodeServerFrameAsClient(b)
		if err != nil {
			t.Fatalf("decode %d: %v", i+1, err)
		}
		se, ok := f.(wire.ServerError)
		if !ok {
			t.Fatalf("decode %d: got %T, want wire.ServerError", i+1, f)
		}
		// Ruling 1 → B: the sentinel has no constant, and IsUnknown is how a
		// client asks. Valid stays false, so nothing treats it as a real code.
		if se.Code.Valid() || !se.Code.IsUnknown() {
			t.Fatalf("decode %d: code %q, Valid()=%v IsUnknown()=%v; want false, true", i+1, se.Code, se.Code.Valid(), se.Code.IsUnknown())
		}
		// Every other field survives, retryable included.
		if !se.Retryable || se.Message != "monthly voice minutes exhausted" {
			t.Fatalf("decode %d: other fields lost: %+v", i+1, se)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	want := `wire: ErrorCode: unknown value "quota_exceeded" decoded as unknown (client wire_version 1)`
	if len(reports) != 1 || reports[0] != want {
		t.Fatalf("reports = %q\nwant exactly one: %q", reports, want)
	}
}

func TestADefinedValueIsNotUnknownAndIsNotReported(t *testing.T) {
	reported := false
	wire.SetOnUnknownWireValue(func(string) { reported = true })
	t.Cleanup(func() { wire.SetOnUnknownWireValue(nil) })

	f, err := wire.DecodeServerFrameAsClient([]byte(`{"type":"resync_required","reason":"cursor_too_old","log_seq":1}`))
	if err != nil {
		t.Fatal(err)
	}
	r := f.(wire.ServerResyncRequired)
	if !r.Reason.Valid() || r.Reason.IsUnknown() || r.Reason != wire.ResyncReasonCursorTooOld {
		t.Fatalf("reason %q: a defined value must decode as itself", r.Reason)
	}
	if reported {
		t.Fatal("a defined value was reported as unknown")
	}
}

// Every constraint that is not a client-open enum is enforced on the client
// side exactly as on the server. One frame carries both an undefined code and
// a malformed client_id: the code alone would decode, the client_id must not.
func TestTheClientSideStillEnforcesEveryOtherConstraint(t *testing.T) {
	b := []byte(`{"type":"error","code":"quota_exceeded","message":"x","retryable":false,"client_id":"not-a-uuid"}`)
	_, err := wire.DecodeServerFrameAsClient(b)
	var de *wire.DecodeError
	if !errors.As(err, &de) || de.Path != "ServerFrame[error].client_id" {
		t.Fatalf("got %v, want a DecodeError at ServerFrame[error].client_id", err)
	}
	if _, err := wire.DecodeNamedAsClient("SyncResponse", []byte(`{"log_seq":0,"has_more":false}`)); err == nil {
		t.Fatal("DecodeNamedAsClient accepted a SyncResponse missing required fields")
	}
}

// The hook is read and written under the seen-set's mutex. Run under -race:
// one goroutine decodes values nobody has seen, so every decode reads the hook,
// while this one swaps it.
func TestSwappingTheHookWhileDecodingIsNotARace(t *testing.T) {
	t.Cleanup(func() { wire.SetOnUnknownWireValue(nil) })
	wire.SetOnUnknownWireValue(func(string) {})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			b := fmt.Sprintf(`{"type":"resync_required","reason":"race_probe_%d","log_seq":1}`, i)
			if _, err := wire.DecodeServerFrameAsClient([]byte(b)); err != nil {
				t.Errorf("decode: %v", err)
				return
			}
		}
	}()
	for i := 0; i < 500; i++ {
		n := i
		wire.SetOnUnknownWireValue(func(string) { _ = n })
	}
	<-done
}

/* ------------------------------------------------------------------ *
 * The server never reaches the client side.
 * ------------------------------------------------------------------ */

const wirePkgPath = "github.com/magos/catenary/internal/wire"

// The one directory whose code acts as a client. An allowlist, not a denylist:
// a new package anywhere else in the module is refused by default.
const clientDir = "internal/client/"

// Not this module: server/ and spike/ are separate modules, web/ and dart/ are
// other languages. server/cmd/conformance runs the client pass legitimately and
// server/cmd/r1client is a spike; neither is a decode the server runs. Matched
// against the path relative to the root, as the CANT-83 guard does.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "web": true, "dart": true,
	"server": true, "spike": true,
}

func TestOnlyInternalClientCallsTheClientSide(t *testing.T) {
	offences := scanTree(t, moduleRoot(t))
	if len(offences) == 0 {
		return
	}
	t.Errorf(`%d mention(s) of a wire.*AsClient entrypoint outside %s:

%s

The client side decodes a client-open enum value this schema version does not
define to "unknown" instead of refusing it. The server is the trust boundary
and refuses it (CANT-74 ruling 2), so server code decodes through the strict
entrypoints — DecodeServerFrame, DecodeNamed, UnmarshalJSON — and never through
these. If new code genuinely acts as a client, it belongs in %s.`,
		len(offences), clientDir, strings.Join(offences, "\n"), clientDir)
}

// TestTheClientSideGuardBites watches the guard above fire. A guard that has
// only ever run against a clean tree is one nobody has seen work.
func TestTheClientSideGuardBites(t *testing.T) {
	for _, tc := range []struct {
		name, at, src string
		want          int
	}{
		{
			name: "a handler decoding as a client",
			at:   "internal/api/planted.go",
			src: `package api

import "github.com/magos/catenary/internal/wire"

func decode(b []byte) (wire.ServerFrame, error) { return wire.DecodeServerFrameAsClient(b) }
`,
			want: 1,
		},
		{
			name: "an aliased import in a package no list names",
			at:   "internal/somewhere/new/planted.go",
			src: `package somewhere

import w "github.com/magos/catenary/internal/wire"

var f = w.DecodeNamedAsClient
`,
			want: 1,
		},
		{
			name: "the entrypoint named as a value, not called, still counts",
			at:   "cmd/catenary/planted.go",
			src: `package main

import "github.com/magos/catenary/internal/wire"

var decoders = []func([]byte) (wire.ServerFrame, error){wire.DecodeServerFrame, wire.DecodeServerFrameAsClient}
`,
			want: 1,
		},
		{
			name: "internal/client is the allowlist",
			at:   "internal/client/planted.go",
			src: `package client

import "github.com/magos/catenary/internal/wire"

var f = wire.DecodeServerFrameAsClient
var g = wire.DecodeNamedAsClient
`,
			want: 0,
		},
		{
			name: "comments and strings are not calls",
			at:   "internal/api/planted.go",
			src: `package api

// wire.DecodeServerFrameAsClient is the client's, not ours.
const note = "wire.DecodeNamedAsClient"
`,
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			p := filepath.Join(root, filepath.FromSlash(tc.at))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			got := scanTree(t, root)
			if len(got) != tc.want {
				t.Errorf("got %d offence(s), want %d\n%s", len(got), tc.want, strings.Join(got, "\n"))
			}
		})
	}
}

// scanTree walks one module tree and returns every wire.*AsClient selector in
// a non-test file outside internal/client. Test files are exempt: a server test
// that decodes as a client is checking the client side, not running it.
func scanTree(t *testing.T, root string) []string {
	t.Helper()

	var offences []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDirs[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		if strings.HasPrefix(rel, clientDir) {
			return nil
		}
		offences = append(offences, scanFile(t, path, rel)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return offences
}

func scanFile(t *testing.T, path, rel string) []string {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	local := wireImportName(t, rel, f)
	if local == "" {
		return nil
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != local || !strings.HasSuffix(sel.Sel.Name, "AsClient") {
			return true
		}
		out = append(out, "  "+rel+":"+strconv.Itoa(fset.Position(sel.Pos()).Line)+": "+local+"."+sel.Sel.Name)
		return true
	})
	return out
}

// wireImportName returns the local name a file binds the wire package to, or
// "" if it does not import it. A dot-import would put the entrypoints in scope
// as bare identifiers and defeat the selector check, so it is refused.
func wireImportName(t *testing.T, rel string, f *ast.File) string {
	t.Helper()

	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != wirePkgPath {
			continue
		}
		if imp.Name == nil {
			return "wire"
		}
		switch imp.Name.Name {
		case ".":
			t.Fatalf("%s dot-imports the wire package, which defeats this guard; import it normally", rel)
		case "_":
			return ""
		}
		return imp.Name.Name
	}
	return ""
}

// moduleRoot walks up to the go.mod that owns this package rather than
// assuming a depth.
func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
