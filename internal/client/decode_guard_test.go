package client

// CANT-177 ruling 2 → A: every receive site decodes AS A CLIENT. internal/wire's
// guard holds the client side to this package; this one holds this package to
// the client side, so a fifth receive site cannot quietly come back strict.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const clientWirePkg = "github.com/magos/catenary/internal/wire"

// strictEntrypoints are the server's decoders for what a server SENDS. Decoding
// a client frame (DecodeClientFrame) is not a receive site, and is not here.
var strictEntrypoints = map[string]bool{"DecodeServerFrame": true, "DecodeNamed": true}

// serverRootTypes are what a server sends this package as a response body. A
// json.Unmarshal into one runs its strict UnmarshalJSON. Server frame types are
// held by prefix: each has one too.
var serverRootTypes = map[string]bool{
	"SyncResponse": true, "EnrollResponse": true, "RefreshResponse": true, "DeviceListResponse": true,
}

func isServerRootType(name string) bool {
	return serverRootTypes[name] || strings.HasPrefix(name, "Server")
}

// strictExempt is the one function that implements Faults.StrictDecode, named
// with its receiver so a free function of the same name is not exempt.
const strictExempt = "Faults.decodeFromServer"

func TestEveryReceiveSiteDecodesAsAClient(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	offences := scanClientDir(t, dir)
	if len(offences) == 0 {
		return
	}
	t.Errorf(`%d strict decode(s) of what a server sends, outside %s:

%s

A Go client decodes AS A CLIENT (CANT-177): a client-open enum value a later
server adds decodes to "unknown" in TypeScript and Dart, and the strict decoder
refuses the whole frame or page that carries it — an error dropped before the
close it explains, a /sync page refused on every retry. Decode through
Faults.decodeFromServer (or decodeResponse), which is the client side unless
Faults.StrictDecode, the negative control, says otherwise.`,
		len(offences), strictExempt, strings.Join(offences, "\n"))
}

// TestTheReceiveSiteGuardBites watches the guard above fire. A guard that has
// only ever run against a clean tree is one nobody has seen work.
func TestTheReceiveSiteGuardBites(t *testing.T) {
	for _, tc := range []struct {
		name, file, src string
		want            int
	}{
		{
			name: "a strict frame decode in a function not so named",
			file: "planted.go",
			src: `package client

import "github.com/magos/catenary/internal/wire"

func readFrame(b []byte) (wire.ServerFrame, error) { return wire.DecodeServerFrame(b) }
`,
			want: 1,
		},
		{
			name: "a strict named decode through an aliased import",
			file: "planted.go",
			src: `package client

import w "github.com/magos/catenary/internal/wire"

var f = w.DecodeNamed
`,
			want: 1,
		},
		{
			name: "json.Unmarshal into a declared server root",
			file: "planted.go",
			src: `package client

import (
	"encoding/json"

	"github.com/magos/catenary/internal/wire"
)

func page(b []byte) (wire.SyncResponse, error) {
	var page wire.SyncResponse
	err := json.Unmarshal(b, &page)
	return page, err
}
`,
			want: 1,
		},
		{
			name: "json.Unmarshal into a literal, and a Decoder into new(...)",
			file: "planted.go",
			src: `package client

import (
	"encoding/json"
	"io"

	"github.com/magos/catenary/internal/wire"
)

func a(b []byte) error { return json.Unmarshal(b, &wire.RefreshResponse{}) }
func b(r io.Reader) error { return json.NewDecoder(r).Decode(new(wire.EnrollResponse)) }
func c(b []byte) error { var e wire.ServerError; return e.UnmarshalJSON(b) }
`,
			want: 3,
		},
		{
			name: "a free function named like the exemption is not it",
			file: "planted.go",
			src: `package client

import "github.com/magos/catenary/internal/wire"

func decodeFromServer(b []byte) (wire.ServerFrame, error) { return wire.DecodeServerFrame(b) }
`,
			want: 1,
		},
		{
			name: "the exemption, the client side, and an error body that is no wire type",
			file: "planted.go",
			src: `package client

import (
	"encoding/json"

	"github.com/magos/catenary/internal/wire"
)

type Faults struct{ StrictDecode bool }

func (f Faults) decodeFromServer(name string, b []byte) (any, error) {
	if f.StrictDecode {
		if name == "ServerFrame" {
			return wire.DecodeServerFrame(b)
		}
		return wire.DecodeNamed(name, b)
	}
	return wire.DecodeNamedAsClient(name, b)
}

func retry(b []byte) string {
	var e struct{ Retry string }
	_ = json.Unmarshal(b, &e)
	return e.Retry
}
`,
			want: 0,
		},
		{
			name: "test files are exempt",
			file: "planted_test.go",
			src: `package client

import "github.com/magos/catenary/internal/wire"

var f = wire.DecodeServerFrame
`,
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := scanClientDir(t, dir); len(got) != tc.want {
				t.Errorf("got %d offence(s), want %d\n%s", len(got), tc.want, strings.Join(got, "\n"))
			}
		})
	}
}

// scanClientDir returns every strict decode of a server's output in a non-test
// .go file directly in dir.
func scanClientDir(t *testing.T, dir string) []string {
	t.Helper()

	var offences []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.Type()&fs.ModeType != 0 || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		offences = append(offences, scanClientFile(t, filepath.Join(dir, name), name)...)
	}
	return offences
}

func scanClientFile(t *testing.T, path, rel string) []string {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	local := ""
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err != nil || p != clientWirePkg {
			continue
		}
		local = "wire"
		if imp.Name != nil {
			if imp.Name.Name == "." {
				t.Fatalf("%s dot-imports the wire package, which defeats this guard; import it normally", rel)
			}
			local = imp.Name.Name
		}
	}
	if local == "" || local == "_" {
		return nil
	}

	// mentionsRoot is true of a type expression naming a server root anywhere
	// in it: wire.SyncResponse, *wire.SyncResponse, struct{ P wire.SyncResponse }.
	mentionsRoot := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == local && isServerRootType(sel.Sel.Name) {
					found = true
				}
			}
			return !found
		})
		return found
	}
	// Names declared with such a type, anywhere in the file: a source shape,
	// not a type check, and deliberately over-wide — a name reused for a
	// server root anywhere in the file is flagged wherever it is decoded into.
	rooted := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ValueSpec:
			for i, id := range n.Names {
				if (n.Type != nil && mentionsRoot(n.Type)) || (i < len(n.Values) && mentionsRoot(n.Values[i])) {
					rooted[id.Name] = true
				}
			}
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for i, lhs := range n.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && i < len(n.Rhs) && len(n.Lhs) == len(n.Rhs) {
						if _, isCall := n.Rhs[i].(*ast.CallExpr); (!isCall || isNewCall(n.Rhs[i])) && mentionsRoot(n.Rhs[i]) {
							rooted[id.Name] = true
						}
					}
				}
			}
		case *ast.Field:
			if mentionsRoot(n.Type) {
				for _, id := range n.Names {
					rooted[id.Name] = true
				}
			}
		}
		return true
	})
	// isTarget is an argument that decodes into a server root: &x, x, &T{},
	// new(T).
	isTarget := func(e ast.Expr) bool {
		if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
			e = u.X
		}
		switch e := e.(type) {
		case *ast.Ident:
			return rooted[e.Name]
		case *ast.CompositeLit:
			return mentionsRoot(e.Type)
		case *ast.CallExpr:
			return isNewCall(e) && mentionsRoot(e)
		}
		return false
	}

	var out []string
	report := func(pos token.Pos, what string) {
		out = append(out, "  "+rel+":"+strconv.Itoa(fset.Position(pos).Line)+": "+what)
	}
	for _, decl := range f.Decls {
		exempt := false
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv != nil && len(fd.Recv.List) == 1 {
			if recv, ok := fd.Recv.List[0].Type.(*ast.Ident); ok && recv.Name+"."+fd.Name.Name == strictExempt {
				exempt = true
			}
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && x.Name == local && strictEntrypoints[n.Sel.Name] && !exempt {
					report(n.Pos(), local+"."+n.Sel.Name)
				}
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "Unmarshal", "Decode":
					for _, a := range n.Args {
						if isTarget(a) {
							report(n.Pos(), sel.Sel.Name+" into a server root type")
							break
						}
					}
				case "UnmarshalJSON":
					if isTarget(sel.X) {
						report(n.Pos(), "UnmarshalJSON on a server root type")
					}
				}
			}
			return true
		})
	}
	return out
}

func isNewCall(e ast.Expr) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := c.Fun.(*ast.Ident)
	return ok && id.Name == "new"
}
