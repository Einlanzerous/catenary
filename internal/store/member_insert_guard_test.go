package store

// CANT-257 — the add-member guard (CANT-254 ruling 0, B).
//
// The wire schema says of a `self` conversation: "A `self` conversation never
// gains a member and is never promoted: nothing in the server adds one." This
// test is how that sentence stays true. It is a SOURCE-SHAPE test in the family
// of metadata_guard_test.go and errorcode_guard_test.go: it parses every
// non-test .go file under internal/ and cmd/ with go/parser, finds every string
// literal containing `INSERT INTO conversation_members`, and fails unless each
// sits inside a function on the allow-list below.
//
// WHY AN ALLOW-LIST OF FUNCTIONS. Today there are three: findOrCreateDirect
// (two members, at creation), findOrCreateSelf (one) and createGroup (the
// creator and the people named, fixed at creation, CANT-268). The day anyone writes a
// fourth, whatever kind that function means to serve, this goes red, and the
// author has to add it here and so to read the self rule above. It does not stop
// SQL run outside this codebase, which nothing here can.
//
// It needs no database: `go test ./internal/store -run TestConversationMemberInsertSites`.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// memberInsertAllowed maps a file (slash-separated, relative to the module
// root) to the functions in it that may insert a conversation_members row.
var memberInsertAllowed = map[string][]string{
	"internal/store/metadata.go": {"findOrCreateDirect", "findOrCreateSelf", "createGroup"},
}

// memberInsertSite is one string literal containing the statement.
type memberInsertSite struct {
	File string
	Func string // "" when the literal is outside any function
	Line int
}

func (s memberInsertSite) String() string {
	fn := s.Func
	if fn == "" {
		fn = "(package level)"
	}
	return s.File + ":" + strconv.Itoa(s.Line) + " in " + fn
}

// memberInsertSitesIn parses one file and returns every site in it.
func memberInsertSitesIn(t *testing.T, rel string, src []byte) []memberInsertSite {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	var out []memberInsertSite
	visit := func(fn string) func(ast.Node) bool {
		return func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if reInsertCM.MatchString(s) {
				out = append(out, memberInsertSite{File: rel, Func: fn, Line: fset.Position(lit.Pos()).Line})
			}
			return true
		}
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			ast.Inspect(fd, visit(fd.Name.Name))
		} else {
			ast.Inspect(d, visit(""))
		}
	}
	return out
}

// memberInsertSitesUnder walks internal/ and cmd/ below root.
func memberInsertSitesUnder(t *testing.T, root string) []memberInsertSite {
	t.Helper()
	var out []memberInsertSite
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			out = append(out, memberInsertSitesIn(t, filepath.ToSlash(rel), src)...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return out
}

// disallowedMemberInserts returns the sites not on the allow-list.
func disallowedMemberInserts(sites []memberInsertSite) []memberInsertSite {
	var bad []memberInsertSite
	for _, s := range sites {
		ok := false
		for _, fn := range memberInsertAllowed[s.File] {
			if fn == s.Func {
				ok = true
			}
		}
		if !ok {
			bad = append(bad, s)
		}
	}
	return bad
}

func TestConversationMemberInsertSites(t *testing.T) {
	sites := memberInsertSitesUnder(t, moduleRoot(t))

	if bad := disallowedMemberInserts(sites); len(bad) > 0 {
		var lines []string
		for _, s := range bad {
			lines = append(lines, s.String())
		}
		sort.Strings(lines)
		t.Errorf(`%d statement(s) insert into conversation_members outside the allow-list:

%s

The wire schema promises that a `+"`self`"+` conversation "never gains a member and is
never promoted: nothing in the server adds one" (CANT-254). Every function that
inserts a membership row is therefore named in memberInsertAllowed in this file,
so adding one is a deliberate act. If your function really must add members,
read ConversationKind's description in schema/catenary.wire.v1.schema.json first,
make sure it can never be pointed at a kind = 'self' conversation, and then add
it to the list.`, len(bad), strings.Join(lines, "\n"))
	}

	// The list must also be TRUE: both named functions still insert, so a rename
	// cannot quietly turn the allow-list into a list of functions that no longer
	// exist while the real site sits somewhere unlisted-but-matching.
	seen := map[string]bool{}
	for _, s := range sites {
		seen[s.File+"#"+s.Func] = true
	}
	for file, fns := range memberInsertAllowed {
		for _, fn := range fns {
			if !seen[file+"#"+fn] {
				t.Errorf("%s is allow-listed to insert members but no longer does; update memberInsertAllowed", file+" "+fn)
			}
		}
	}
}

// TestTheMemberInsertGuardBites is why the test above is worth having: a third
// function containing the statement is reported, whatever it is called and
// however the statement is spelled across lines and case.
func TestTheMemberInsertGuardBites(t *testing.T) {
	src := []byte("package store\n\n" +
		"func findOrCreateDirect() { _ = `INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)` }\n" +
		"func findOrCreateSelf() { _ = `INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)` }\n")
	file := "internal/store/metadata.go"
	if bad := disallowedMemberInserts(memberInsertSitesIn(t, file, src)); len(bad) != 0 {
		t.Fatalf("the two allowed functions were flagged: %v", bad)
	}

	withThird := append(append([]byte{}, src...), []byte(
		"func addToRoom() {\n\t_ = `\n\t\tinsert into conversation_members\n\t\t(conversation_id, user_id) VALUES ($1, $2)`\n}\n")...)
	bad := disallowedMemberInserts(memberInsertSitesIn(t, file, withThird))
	if len(bad) != 1 || bad[0].Func != "addToRoom" {
		t.Fatalf("a third function containing the statement was not reported exactly once: %v", bad)
	}

	// The same statement in another file is flagged even inside an allowed name.
	other := disallowedMemberInserts(memberInsertSitesIn(t, "internal/store/other.go", src))
	if len(other) != 2 {
		t.Errorf("an allowed function name in the wrong file was not flagged: %v", other)
	}

	// A comment is not a statement.
	commented := []byte("package store\n\n// INSERT INTO conversation_members is documented here.\nfunc doc() {}\n")
	if got := memberInsertSitesIn(t, file, commented); len(got) != 0 {
		t.Errorf("a comment was reported as a statement: %v", got)
	}
}
