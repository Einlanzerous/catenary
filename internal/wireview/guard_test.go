package wireview

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

// CANT-84 criterion 12 — wireview.Message is the ONLY function in the service
// module that constructs a wire.Message.
//
// CANT-75's Done-when says the socket's Message and REST's must be
// indistinguishable, and that is true only if one function produces both. Two
// transports each assembling their own is Invariant 2's failure class arriving
// through the back door — and it would not fail a test, it would just serve
// slightly different objects to the two clients.
//
// SCOPED TO THE SERVICE MODULE, deliberately. server/cmd/r1rig constructs ONE
// wire.Message literal today, at main.go:77 — it is a spike rig, not a
// transport, and a repo-wide guard would have failed on the day it was written.
// (Earlier revisions of this comment said two. Line 112 is `[]wire.Message{}`,
// an empty slice, which constructs nothing and which this scan would not count
// anyway. Comments in this repository are load-bearing and that one was
// checkable.)

const (
	wirePkgPath = "github.com/magos/catenary/internal/wire"
	// The one file allowed to build one.
	mapperFile = "internal/wireview/message.go"
)

// Not this module: server/ and spike/r6-purser are separate modules, web/ and
// dart/ are other languages.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "web": true, "dart": true,
	"server": true, "spike": true,
}

func TestOnlyWireviewConstructsAWireMessage(t *testing.T) {
	offences := scanForMessageLiterals(t, moduleRoot(t))
	if len(offences) == 0 {
		return
	}
	t.Errorf(`%d place(s) outside %s construct a wire.Message:

%s

One function produces the Message both transports return, or the socket and
REST drift into serving different objects for the same row — which no test
catches, because each is internally consistent. Call wireview.Message instead;
if it cannot give you what you need, widen it rather than assembling a second
one.

_test.go files are exempt: a test building an expected value is CHECKING the
mapping, not making one. server/ is exempt too — it is a separate module and
its spike rigs are not transports.`, len(offences), mapperFile, strings.Join(offences, "\n"))
}

// And the guard is proved to bite, because one that has only run against a
// clean tree is one nobody has seen work.
func TestTheMessageGuardBites(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{
			name: "a transport assembling its own",
			src: `package api

import "github.com/magos/catenary/internal/wire"

func broadcast() wire.Message {
	return wire.Message{ID: "x", Seq: 1}
}
`,
			want: 1,
		},
		{
			name: "an aliased import does not evade it",
			src: `package api

import w "github.com/magos/catenary/internal/wire"

var m = w.Message{}
`,
			want: 1,
		},
		{
			name: "a pointer literal counts",
			src: `package api

import "github.com/magos/catenary/internal/wire"

var m = &wire.Message{}
`,
			want: 1,
		},
		{
			name: "a slice of Messages with elided element types",
			src: `package api

import "github.com/magos/catenary/internal/wire"

func page() []wire.Message {
	return []wire.Message{{ID: "a"}, {ID: "b"}}
}
`,
			// This is how CANT-20's /sync would assemble a page inline.
			want: 2,
		},
		{
			name: "a map of Messages with elided value types",
			src: `package api

import "github.com/magos/catenary/internal/wire"

var byID = map[string]wire.Message{"a": {ID: "a"}}
`,
			want: 1,
		},
		{
			name: "a zero value and field assignment",
			src: `package api

import "github.com/magos/catenary/internal/wire"

func build() wire.Message {
	var m wire.Message
	m.ID = "x"
	return m
}
`,
			want: 1,
		},
		{
			name: "new() and field assignment",
			src: `package api

import "github.com/magos/catenary/internal/wire"

func build() *wire.Message {
	m := new(wire.Message)
	m.ID = "x"
	return m
}
`,
			want: 1,
		},
		{
			name: "an empty slice constructs nothing",
			src: `package api

import "github.com/magos/catenary/internal/wire"

var none = []wire.Message{}
`,
			// r1rig:112's shape. A slice OF Messages holding none of them.
			want: 0,
		},
		{
			name: "naming the type is not constructing one",
			src: `package api

import "github.com/magos/catenary/internal/wire"

func send(m wire.Message) wire.Message { return m }
`,
			want: 0,
		},
		{
			name: "another wire type is not a Message",
			src: `package api

import "github.com/magos/catenary/internal/wire"

var a = wire.ImageAttachment{}
`,
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "planted.go"), []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := scanForMessageLiterals(t, root); len(got) != tc.want {
				t.Errorf("got %d offence(s), want %d: %v", len(got), tc.want, got)
			}
		})
	}
}

func scanForMessageLiterals(t *testing.T, root string) []string {
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
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if rel == filepath.FromSlash(mapperFile) {
			return nil
		}

		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", rel, parseErr)
		}
		local := wireImportName(t, rel, f)
		if local == "" {
			return nil
		}
		// FOUR SHAPES, because a syntax check that knows one is a guard proved
		// against its own idiom and no other. The first version matched only a
		// CompositeLit whose Type is a SelectorExpr, and a review ran the scan
		// over planted sources to show the rest walking past:
		//
		//	wire.Message{…}                        caught
		//	[]wire.Message{{…}, {…}}               NOT — inner literals elide the type
		//	map[string]wire.Message{"a": {…}}      NOT — same
		//	var m wire.Message; m.ID = …           NOT — no CompositeLit at all
		//	new(wire.Message)                      NOT — a CallExpr
		//
		// The slice form is not contrived: CANT-20's /sync returns a page, and
		// `[]wire.Message{{…}}` is how a handler assembles one inline. `var m`
		// then field assignment is the other everyday idiom. Either would have
		// landed a second assembler in internal/api under a green guard.
		//
		// WHAT THIS STILL DOES NOT COVER, stated rather than left implied: a
		// Message obtained from this package and then MUTATED names no type and
		// is invisible here. That is a different offence from constructing one,
		// and closing it needs go/types over the whole module rather than a
		// syntax walk — worth doing if a second assembler ever appears by that
		// route, and not worth a dependency before then.
		isMessageType := func(e ast.Expr) bool {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Message" {
				return false
			}
			pkg, ok := sel.X.(*ast.Ident)
			return ok && pkg.Name == local
		}
		note := func(n ast.Node, what string) {
			offences = append(offences,
				"  "+rel+":"+strconv.Itoa(fset.Position(n.Pos()).Line)+": "+what)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if isMessageType(node.Type) {
					note(node, local+".Message{…}")
					return true
				}
				// A slice, array or map OF Messages: the elements elide the
				// type, so each one is a construction with no type node of its
				// own to match.
				var elem ast.Expr
				switch t := node.Type.(type) {
				case *ast.ArrayType:
					elem = t.Elt
				case *ast.MapType:
					elem = t.Value
				}
				if elem != nil && isMessageType(elem) {
					for _, el := range node.Elts {
						inner, ok := el.(*ast.CompositeLit)
						if !ok {
							// map form: key: {…}
							if kv, isKV := el.(*ast.KeyValueExpr); isKV {
								inner, ok = kv.Value.(*ast.CompositeLit)
							}
						}
						if ok && inner != nil {
							note(inner, local+".Message{…} (elided, inside a composite)")
						}
					}
				}
			case *ast.ValueSpec:
				// var m wire.Message — a zero value waiting for field
				// assignments, which is construction in two steps.
				if isMessageType(node.Type) {
					note(node, "var … "+local+".Message")
				}
			case *ast.CallExpr:
				if fn, ok := node.Fun.(*ast.Ident); ok && fn.Name == "new" &&
					len(node.Args) == 1 && isMessageType(node.Args[0]) {
					note(node, "new("+local+".Message)")
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return offences
}

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
		if imp.Name.Name == "." {
			t.Fatalf("%s dot-imports the wire package, which defeats this guard", rel)
		}
		if imp.Name.Name == "_" {
			return ""
		}
		return imp.Name.Name
	}
	return ""
}

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
