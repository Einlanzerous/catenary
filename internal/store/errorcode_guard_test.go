package store

// CANT-83 criterion 2 — the guard that keeps senderror.go the only file that
// decides a send's wire code.
//
// It bans IDENTIFIERS, not string literals, and that is the whole point. A
// handler writing
//
//	case errors.Is(err, store.ErrNotFound):
//	    return wire.ErrorCodeConversationNotFound
//
// writes no literal anywhere, and is exactly the second decision this forbids.
// So the check parses Go rather than grepping text: a mention inside a comment
// or a string is not a decision, and a selector on the wire package is.
//
// The shape is borrowed from the phrase guard in verify.sh's CANT-13 step,
// which exists because one wrong phrasing of log_seq reached seven places
// before anyone noticed. A rule with no mechanism is a rule that holds until
// the first busy afternoon. (Naming that phrase here would have been its
// eighth and ninth places — the guard caught this comment, which is as good a
// demonstration of the pattern as the probes below.)

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

const (
	wirePkgPath = "github.com/magos/catenary/internal/wire"

	// The table file. The one place allowed to decide.
	tableFile = "internal/store/senderror.go"

	// The generated package DEFINES the constants; naming them there is not a
	// second decision.
	generatedFile = "internal/wire/generated.go"
)

// The two codes any file may name, exempted BY NAME rather than by pattern.
//
// Post-approval refinement, decided on CANT-18: unauthorized and
// wire_version_unsupported are never store causes. They belong to CANT-22's
// handshake and CANT-29's refresh rotation, and a guard banning every mention
// would stop those tickets emitting them at all.
//
// The exemption is narrow on purpose. The table exists so that one cause
// cannot acquire two codes, and divergence requires a CHOICE between codes.
// wire_version_unsupported has exactly one producer — the socket hello, since
// REST negotiates no wire version — so it has nothing to diverge from.
// unauthorized has two producers but admits only one candidate code, so the
// arbitration the table performs does not arise.
var exemptCodes = map[string]string{
	"ErrorCodeUnauthorized":           "CANT-22's auth handshake and CANT-29's refresh rotation",
	"ErrorCodeWireVersionUnsupported": "CANT-22's socket hello",
}

// Directories that are not this module. server/ and spike/r6-purser are
// separate modules; web/ and dart/ are other languages entirely.
//
// Scoped to the service module for the same reason criterion 9 scopes the
// wire.Message guard there: a spike binary deciding a code for itself cannot
// make the two transports disagree, because a spike is not a transport.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "web": true, "dart": true,
	"server": true, "spike": true,
}

func TestOnlyOneFileDecidesASendErrorCode(t *testing.T) {
	offences := scanTree(t, moduleRoot(t))
	if len(offences) == 0 {
		return
	}
	t.Errorf(`%d file(s) outside %s decide a send's wire code:

%s

Every send refusal gets its code in ONE place, so the socket and REST cannot
produce different codes for the same cause. If you need a new refusal, add a
cause and a row to the table in %s and return it from the store — do not map an
error to a code at the point of use.

If you are CANT-22 or CANT-29 and need a transport-level code, ErrorCodeUnauthorized
and ErrorCodeWireVersionUnsupported are exempt by name. If some future producer
faces a genuine CHOICE between two codes for one cause, the fix is to WIDEN THE
TABLE, not to widen the exemption — the exemption is safe only because neither
exempt code admits a choice.`,
		len(offences), tableFile, strings.Join(offences, "\n"), tableFile)
}

// TestTheCodeGuardBites is why the test above is worth having. A guard that has
// only ever run against a clean tree is a guard nobody has seen work, and the
// phrase guard in verify.sh's CANT-13 step carries a planted probe for
// exactly this reason.
//
// Three probes, because the guard makes three claims: it catches a decision, it
// lets the two exempt codes through, and it reads IDENTIFIERS rather than text.
func TestTheCodeGuardBites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		src    string
		want   int
		reason string
	}{
		{
			name: "a handler mapping a cause to a code",
			src: `package handler

import "github.com/magos/catenary/internal/wire"

func codeFor(notAMember bool) wire.ErrorCode {
	if notAMember {
		return wire.ErrorCodeNotAMember
	}
	return wire.ErrorCodeInternal
}
`,
			// the return type, plus both constants
			want:   3,
			reason: "the exact second decision the guard exists to forbid, and it writes no string literal",
		},
		{
			name: "an aliased import does not evade it",
			src: `package handler

import w "github.com/magos/catenary/internal/wire"

var c = w.ErrorCodeMessageTooLarge
`,
			want:   1,
			reason: "the guard resolves the local name from the import rather than assuming \"wire\"",
		},
		{
			name: "the two transport codes are exempt",
			src: `package transport

import "github.com/magos/catenary/internal/wire"

var a = wire.ErrorCodeUnauthorized
var b = wire.ErrorCodeWireVersionUnsupported
`,
			want:   0,
			reason: "CANT-22 and CANT-29 must be able to emit these",
		},
		{
			name: "comments and strings are not decisions",
			src: `package doc

// wire.ErrorCodeNotAMember is what a non-member send gets.
const note = "wire.ErrorCodeInternal"
`,
			want:   0,
			reason: "this is why the guard parses Go instead of grepping",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "planted.go"), []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			got := scanTree(t, root)
			if len(got) != tc.want {
				t.Errorf("got %d offence(s), want %d — %s\n%s",
					len(got), tc.want, tc.reason, strings.Join(got, "\n"))
			}
		})
	}
}

// scanTree walks one module tree and returns every banned mention in it.
func scanTree(t *testing.T, root string) []string {
	t.Helper()

	var offences []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// _test.go files are exempt, and the exemption is stated rather than
		// implied: a test asserting that a cause yields not_a_member is
		// CHECKING the decision, not making one. This file is itself the
		// reason that has to be true.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if rel == filepath.FromSlash(tableFile) || rel == filepath.FromSlash(generatedFile) {
			return nil
		}
		offences = append(offences, scanFileForCodeDecisions(t, path, rel)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return offences
}

// scanFileForCodeDecisions returns one line per banned mention.
func scanFileForCodeDecisions(t *testing.T, path, rel string) []string {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}

	local := wireImportName(t, rel, f)
	if local == "" {
		return nil // does not import the wire package at all
	}

	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != local {
			return true
		}
		name := sel.Sel.Name
		// The bare type counts. `func codeFor(...) wire.ErrorCode` in a
		// transport is a second decision point even before it has a body.
		if name != "ErrorCode" && !strings.HasPrefix(name, "ErrorCode") {
			return true
		}
		if _, exempt := exemptCodes[name]; exempt {
			return true
		}
		pos := fset.Position(sel.Pos())
		out = append(out, "  "+rel+":"+strconv.Itoa(pos.Line)+": "+local+"."+name)
		return true
	})
	return out
}

// wireImportName returns the local name the file binds the wire package to, or
// "" if it does not import it.
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
			// A dot-import puts ErrorCodeNotAMember in scope as a bare
			// identifier and defeats the selector check entirely. Refused
			// rather than handled: nothing in this tree wants one, and a guard
			// with a documented blind spot is not a guard.
			t.Fatalf("%s dot-imports the wire package, which defeats this guard; import it normally", rel)
		case "_":
			return ""
		}
		return imp.Name.Name
	}
	return ""
}

// moduleRoot walks up from the package directory to the go.mod that owns it,
// rather than assuming a depth. A test that hardcodes "../.." is a test that
// silently scans the wrong tree the day the package moves — which is what
// CANT-82 just did to the wire package.
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
