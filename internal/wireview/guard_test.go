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
			name: "make with a known length, then field assignment",
			src: `package api

import "github.com/magos/catenary/internal/wire"

func page(rows []int) []wire.Message {
	out := make([]wire.Message, len(rows))
	for i := range rows {
		out[i].Seq = 1
	}
	return out
}
`,
			// The shape CANT-20's /sync reaches for: the page length is known
			// from the row count, so make() is the natural idiom.
			want: 1,
		},
		{
			name: "a slice of POINTERS with elided &T",
			src: `package api

import "github.com/magos/catenary/internal/wire"

var page = []*wire.Message{{ID: "a"}, {ID: "b"}}
`,
			want: 2,
		},
		{
			name: "a map of pointers with elided &T",
			src: `package api

import "github.com/magos/catenary/internal/wire"

var byID = map[string]*wire.Message{"a": {ID: "a"}}
`,
			want: 1,
		},
		{
			name: "an array declaration is N zero Messages",
			src: `package api

import "github.com/magos/catenary/internal/wire"

func build() wire.Message {
	var arr [2]wire.Message
	arr[0].ID = "x"
	return arr[0]
}
`,
			want: 1,
		},
		{
			name: "a nil slice declaration holds none",
			src: `package api

import "github.com/magos/catenary/internal/wire"

func collect() []wire.Message {
	var out []wire.Message
	return out
}
`,
			// A slice header, not a Message. Appending one built by wireview is
			// exactly what a transport SHOULD do.
			want: 0,
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
			// MATCHED ON THE PATH RELATIVE TO ROOT, not the basename. A
			// basename match exempts a directory so named at ANY depth, so a
			// future internal/api/server/ would go unscanned in silence — the
			// same fix internal/store's guard already carries.
			if skipDirs[filepath.ToSlash(rel)] {
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
		// A SYNTAX WALK ENUMERATES SHAPES, AND THIS ONE ENUMERATES SEVEN.
		// That sentence is the honest description and it took two rounds of
		// review to earn: the first version knew one shape, the second knew
		// four, and both times the reviewer found the gap by transcribing this
		// scan into a standalone program and running it over planted sources
		// rather than by reading it.
		//
		//	wire.Message{…}                     round 1
		//	[]wire.Message{{…}, {…}}            round 2 — elements elide the type
		//	map[string]wire.Message{"a": {…}}   round 2
		//	var m wire.Message                  round 2 — no CompositeLit at all
		//	new(wire.Message)                   round 2 — a CallExpr
		//	make([]wire.Message, n)             round 3 — n zero Messages
		//	[]*wire.Message{{…}} / map of *T    round 3 — the &T elision
		//	var arr [2]wire.Message             round 3 — an ARRAY is n values
		//
		// make([]T, n) is the one that mattered. SyncResponse.Messages is
		// []Message and a page's length is known from the row count, so
		// `out := make([]wire.Message, len(rows))` followed by out[i].ID = …
		// is precisely how CANT-20's /sync would assemble a second Message
		// assembler in internal/api, under a green guard.
		//
		// `var s []wire.Message` is deliberately NOT counted: a nil slice holds
		// no Messages. `var arr [2]wire.Message` is, because an array of two IS
		// two zero Messages waiting for field assignment.
		//
		// THE TOTAL ALTERNATIVE, named so the next person does not re-derive
		// it: go/types over the module, flagging any expression whose type is
		// wire.Message originating outside the mapper. That needs no shape list
		// and cannot be enumerated short. It is not here because it wants
		// golang.org/x/tools in a service module that CANT-82 went to some
		// trouble to keep small, and the shapes below cover every idiom anybody
		// has actually proposed. If a third round finds an eighth, that is the
		// signal to pay for it.
		//
		// STILL NOT COVERED, and genuinely out of reach of any syntax walk: a
		// Message obtained from this package and then MUTATED names no type at
		// all. That is a different offence from constructing one.
		// isMessageType unwraps a pointer, because the spec grants the &T
		// elision alongside the T elision: []*wire.Message{{…}} constructs
		// Messages exactly as []wire.Message{{…}} does.
		var isMessageType func(ast.Expr) bool
		isMessageType = func(e ast.Expr) bool {
			if star, ok := e.(*ast.StarExpr); ok {
				return isMessageType(star.X)
			}
			sel, ok := e.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Message" {
				return false
			}
			pkg, ok := sel.X.(*ast.Ident)
			return ok && pkg.Name == local
		}
		// elementIsMessage reports whether a composite/array/map type holds
		// Messages, and whether it is an ARRAY (a fixed number of values) as
		// opposed to a slice or map (which start empty).
		elementIsMessage := func(e ast.Expr) (holds bool, fixedLen bool) {
			switch t := e.(type) {
			case *ast.ArrayType:
				return isMessageType(t.Elt), t.Len != nil
			case *ast.MapType:
				return isMessageType(t.Value), false
			}
			return false, false
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
				if holds, _ := elementIsMessage(node.Type); holds {
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
				if node.Type != nil && isMessageType(node.Type) {
					note(node, "var … "+local+".Message")
					return true
				}
				// var arr [2]wire.Message — an array IS its elements, so this
				// is two zero Messages. A slice or map declared this way holds
				// none and is not counted.
				if holds, fixedLen := elementIsMessage(node.Type); holds && fixedLen {
					note(node, "var … [N]"+local+".Message")
				}
			case *ast.CallExpr:
				fn, ok := node.Fun.(*ast.Ident)
				if !ok || len(node.Args) == 0 {
					return true
				}
				switch fn.Name {
				case "new":
					if isMessageType(node.Args[0]) {
						note(node, "new("+local+".Message)")
					}
				case "make":
					// make([]wire.Message, n) constructs n zero Messages, and
					// the field assignments follow. This is the shape CANT-20's
					// /sync reaches for, because a page's length is known.
					if holds, _ := elementIsMessage(node.Args[0]); holds {
						note(node, "make(…"+local+".Message…)")
					}
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
