package store

// CANT-28 criterion 8 — Authenticate is the ONLY thing that resolves a
// credential, and nothing else reads a token table.
//
// The seam's value is that four refusals are decided in one place: an
// unresolvable credential, an expired one, a revoked device, and a deactivated
// account. A handler that ran its own SELECT against access_tokens would get
// the first two right by accident and the last two wrong by omission — and it
// would pass every test it had, because each check it did write would work.
// That is the CANT-83 argument about error codes applied to authentication:
// one decision, or several that agree until they do not.
//
// This is a source scan rather than a type check because there is nothing to
// type-check. The tables are strings in SQL, so only their names can be
// watched.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The three tables that hold credentials. devices and users are deliberately
// absent: plenty of code legitimately reads those, and it is the token tables
// that carry the refusal logic.
var credentialTables = []string{"access_tokens", "refresh_tokens", "enrollment_tokens"}

func TestOnlyTheStorePackageReadsACredentialTable(t *testing.T) {
	root := storeModuleRoot(t)
	offences := scanForCredentialTables(t, root)
	if len(offences) == 0 {
		return
	}
	t.Errorf(`%d place(s) outside internal/store name a credential table:

%s

Authentication is decided in store.Authenticate and nowhere else. A second
place that resolves a token will check what its author remembered — and the
two that get forgotten are devices.revoked_at and users.deactivated_at, which
are the two that make revocation immediate and make R6's offboard ordering
correct.

Call store.Authenticate. If it cannot answer what you need, widen it rather
than writing a second one.

_test.go files are exempt: a test arranging or inspecting a token row is
CHECKING the model, not making a second decision about it.`,
		len(offences), strings.Join(offences, "\n"))
}

// And the guard is proved to bite, because one that has only run against a
// clean tree is one nobody has seen work.
func TestTheCredentialTableGuardBites(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "a handler running its own lookup",
			src: "package api\n" +
				"var q = `SELECT user_id FROM access_tokens WHERE token_hash = $1`\n",
			want: true,
		},
		{
			name: "split across a string concatenation",
			src: "package api\n" +
				"var q = \"SELECT user_id FROM \" + \"refresh_tokens\"\n",
			want: true,
		},
		{
			name: "an unrelated file",
			src: "package api\n" +
				"var q = `SELECT id FROM messages WHERE conversation_id = $1`\n",
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := namesACredentialTable(tc.src); got != tc.want {
				t.Errorf("namesACredentialTable = %v, want %v", got, tc.want)
			}
		})
	}
}

func namesACredentialTable(src string) bool {
	for _, table := range credentialTables {
		if strings.Contains(src, table) {
			return true
		}
	}
	return false
}

func scanForCredentialTables(t *testing.T, root string) []string {
	t.Helper()
	// Not this module: server/ and spike/ are separate modules, web/ and dart/
	// are other languages, migrations/ is where the tables are CREATED.
	skip := map[string]bool{
		".git": true, "node_modules": true, "web": true, "dart": true,
		"server": true, "spike": true, "migrations": true, "docs": true, "deploy": true,
	}
	var offences []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case !strings.HasSuffix(path, ".go"):
			return nil
		case strings.HasSuffix(path, "_test.go"):
			return nil
		// The store is where the decision lives, so it is the one place
		// allowed to name them.
		case strings.HasPrefix(rel, filepath.Join("internal", "store")+string(filepath.Separator)):
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if namesACredentialTable(string(src)) {
			offences = append(offences, "  "+rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return offences
}

func storeModuleRoot(t *testing.T) string {
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
