package store

// CANT-89 criterion 7 — the guard that makes the bump obligation a SHAPE.
//
// The plan's rev 1 said "every future mutation site must bump" and conceded it
// was mitigated only by there being no site yet. That is the sentence found in
// a post-mortem. The house style is the other way round: Invariant 1 is
// "enforced by the operation's shape rather than by a comment asking callers to
// be careful", and errorcode_guard_test.go and wireview/guard_test.go are both
// already in the tree doing this job. So a write that ought to move a marker
// and does not is a BUILD FAILURE, on the day CANT-75's find-or-create or the
// first rename is written rather than on the day a client shows a stale name.
//
// A TRIGGER WAS CONSIDERED AND REJECTED. Nothing could forget it, but it draws
// log_counter at statement time, which in a multi-statement transaction moves
// the counter draw off "last" — the one lock-order property CANT-89 is
// otherwise careful not to disturb.
//
// WHAT IT MUST NOT CATCH is as load-bearing as what it catches. MarkRead's
// `UPDATE conversation_members … SET read_seq` is the receipt write itself and
// is not a metadata change; SendMessage's `UPDATE conversations SET last_seq`
// is the ordinal draw. Both are legitimate and both live outside metadata.go,
// so the check is per COLUMN rather than per table — a guard that fired on
// either would be removed within a day, and a guard nobody keeps is worse than
// none.
//
// _test.go files are NOT scanned, deliberately. A test has to be able to plant
// a row at the default marker to prove that such a row is invisible to every
// cursor, which is exactly what TestAConversationLeftAtTheDefaultIsInvisible
// does — and that is the assertion justifying the helper's existence. Scanning
// tests would ban the demonstration of the rule.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The one file allowed to write a marker, or to change what a marker describes.
const metadataFile = "internal/store/metadata.go"

// Columns whose change must move a marker, by table.
var metadataColumns = map[string][]string{
	"conversations":        {"name", "kind", "retention_days", "metadata_log_seq"},
	"conversation_members": {"metadata_log_seq"},
	"users":                {"display_name", "metadata_log_seq"},
}

var (
	// Both UPDATE forms, because `INSERT … ON CONFLICT DO UPDATE SET` is the
	// house's own find-or-create idiom (schema_test.go:569) and the plain
	// `UPDATE <table> … SET` pattern does not see it at all: the (\w+) binds
	// to SET and then wants a second SET that is not there.
	reUpdate     = regexp.MustCompile(`(?is)\bUPDATE\s+(\w+)\b(.*?)\bSET\b(.*?)(?:\bFROM\b|\bWHERE\b|\bRETURNING\b|$)`)
	reDoUpdate   = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+(\w+)\b.*?\bON\s+CONFLICT\b.*?\bDO\s+UPDATE\s+SET\b(.*?)(?:\bWHERE\b|\bRETURNING\b|$)`)
	reInsertConv = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+conversations\b`)
	reInsertCM   = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+conversation_members\b`)
	reDeleteCM   = regexp.MustCompile(`(?is)\bDELETE\s+FROM\s+conversation_members\b`)
	reWordBreak  = regexp.MustCompile(`\w+`)
)

// columnsTouched reports which watched columns a SET clause names.
func columnsTouched(table, set string) []string {
	cols, watched := metadataColumns[strings.ToLower(table)]
	if !watched {
		return nil
	}
	words := map[string]bool{}
	for _, w := range reWordBreak.FindAllString(strings.ToLower(set), -1) {
		words[w] = true
	}
	var out []string
	for _, c := range cols {
		if words[c] {
			out = append(out, c)
		}
	}
	return out
}

// offencesIn returns one description per banned write found in the SQL literal.
func offencesIn(sql string) []string {
	var out []string
	for _, m := range reUpdate.FindAllStringSubmatch(sql, -1) {
		for _, c := range columnsTouched(m[1], m[3]) {
			out = append(out, "UPDATE "+strings.ToLower(m[1])+" … SET "+c)
		}
	}
	for _, m := range reDoUpdate.FindAllStringSubmatch(sql, -1) {
		for _, c := range columnsTouched(m[1], m[2]) {
			out = append(out, "INSERT INTO "+strings.ToLower(m[1])+" … DO UPDATE SET "+c)
		}
	}
	// CREATING a conversation is a change too, and the one with no second
	// chance: a row inserted without a draw sits at the DEFAULT 0, which is
	// below every cursor there is, so it is invisible to every client forever.
	// That is what TestAnEmptyConversationIsDiscoverableOnlyIfItsCreationDrew
	// demonstrates, and this is the statement it demonstrates it about.
	//
	// `users` is NOT here: a user becomes visible by joining a conversation,
	// which bumps that conversation, so creation needs no draw and DEFAULT 0
	// is correct for it.
	if reInsertConv.MatchString(sql) {
		out = append(out, "INSERT INTO conversations")
	}
	// Membership itself is the change, so the whole statement is the offence
	// rather than a column within it.
	if reInsertCM.MatchString(sql) {
		out = append(out, "INSERT INTO conversation_members")
	}
	if reDeleteCM.MatchString(sql) {
		out = append(out, "DELETE FROM conversation_members")
	}
	return out
}

func scanForMetadataWrites(t *testing.T, root string) []string {
	t.Helper()
	skip := map[string]bool{
		".git": true, "node_modules": true, "web": true, "dart": true,
		"server": true, "spike": true,
	}
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if skip[rel] || skip[d.Name()] && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.ToSlash(rel) == metadataFile {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, o := range offencesInFile(t, filepath.ToSlash(rel), src) {
			found = append(found, o)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return found
}

// offencesInFile parses Go rather than grepping text, for the same reason
// errorcode_guard_test.go does: it is what tells a SQL statement in a string
// literal from the same words in a comment.
func offencesInFile(t *testing.T, rel string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		for _, o := range offencesIn(s) {
			out = append(out, rel+":"+strconv.Itoa(fset.Position(lit.Pos()).Line)+" — "+o)
		}
		return true
	})
	return out
}

func TestOnlyOneFileMovesAMetadataMarker(t *testing.T) {
	offences := scanForMetadataWrites(t, moduleRoot(t))
	if len(offences) == 0 {
		return
	}
	t.Errorf(`%d write(s) outside %s change metadata without moving its marker:

%s

Anything that changes what /sync serves for a conversation, a member or a user
has to draw a new metadata_log_seq in the same transaction, or the change is
invisible to every client whose cursor is already past it — which is the whole
of CANT-89. Route the write through bumpConversationMetadata,
bumpMemberMetadata or bumpUserMetadata in %s.

If you are adding a column whose change does NOT need to reach a client, add it
to neither this guard nor the marker; if it does, add it to metadataColumns
here at the same time as you add it to the migration.`,
		len(offences), metadataFile, strings.Join(offences, "\n"), metadataFile)
}

// TestTheMetadataGuardBites is why the test above is worth having. The five
// probes are the five claims it makes.
func TestTheMetadataGuardBites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sql    string
		want   int
		reason string
	}{
		{
			name:   "a rename",
			sql:    `UPDATE conversations SET name = $2 WHERE id = $1`,
			want:   1,
			reason: "the first mutation anyone will write, and the one the ticket exists for",
		},
		{
			name:   "a member joining",
			sql:    `INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)`,
			want:   1,
			reason: "membership is in Conversation.member_count, so a join changes what every member is served",
		},
		{
			name:   "a member leaving",
			sql:    `DELETE FROM conversation_members WHERE conversation_id = $1 AND user_id = $2`,
			want:   1,
			reason: "and CANT-90's read_by counts rows in this table, so a departure moves a fraction too",
		},
		{
			name:   "a find-or-create that renames on conflict",
			sql:    `INSERT INTO conversations (id, kind, name, direct_key) VALUES ($1,'direct',$2,$3) ON CONFLICT (direct_key) DO UPDATE SET name = EXCLUDED.name RETURNING id`,
			want:   2,
			reason: "the plain UPDATE pattern cannot see this at all, and it is the house's own find-or-create idiom — the INSERT and the DO UPDATE SET are both offences",
		},
		{
			name:   "creating a conversation without drawing",
			sql:    `INSERT INTO conversations (id, kind, name) VALUES ($1, 'group', $2)`,
			want:   1,
			reason: "a row left at the DEFAULT 0 is below every cursor there is, so it is invisible to every client forever",
		},
		{
			name:   "creating a user is NOT an offence",
			sql:    `INSERT INTO users (id, handle, display_name) VALUES ($1, $2, $3)`,
			want:   0,
			reason: "a user becomes visible by joining a conversation, which bumps that conversation; DEFAULT 0 is correct for users",
		},
		{
			name:   "a display-name change",
			sql:    `UPDATE users SET display_name = $2 WHERE id = $1`,
			want:   1,
			reason: "the users array carries the same promise as conversations and had the same gap",
		},
		{
			name: "MarkRead's own receipt write is NOT an offence",
			sql: `UPDATE conversation_members cm
			         SET read_seq = LEAST(GREATEST(cm.read_seq, $3), c.last_seq)
			        FROM conversations c
			       WHERE c.id = cm.conversation_id`,
			want:   0,
			reason: "per column, not per table — a guard that fired on the receipt write would be deleted within a day",
		},
		{
			name:   "SendMessage's ordinal bump is NOT an offence",
			sql:    `UPDATE conversations SET last_seq = last_seq + 1 WHERE id = $1 RETURNING last_seq`,
			want:   0,
			reason: "last_seq is the per-conversation ordinal source and head_seq is served from it; a send puts the conversation on the page anyway",
		},
	} {
		if got := len(offencesIn(tc.sql)); got != tc.want {
			t.Errorf("%s: %d offence(s), want %d — %s\n  sql: %s", tc.name, got, tc.want, tc.reason, tc.sql)
		}
	}
}
