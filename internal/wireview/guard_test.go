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
// SCOPED TO THE SERVICE MODULE, deliberately. server/cmd/r1rig constructs two
// wire.Message literals today; it is a spike rig, not a transport, and a
// repo-wide guard would have failed on the day it was written.

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
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Message" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != local {
				return true
			}
			offences = append(offences,
				"  "+rel+":"+strconv.Itoa(fset.Position(lit.Pos()).Line)+": "+local+".Message{…}")
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
