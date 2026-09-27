package store

// CANT-141 — otherMemberJoin is spliced into exactly two queries, and its own
// `kind = 'direct'` guard sits inside the LATERAL subquery's own WHERE, with
// `ON true` on the join itself, never the other way round. The second half is
// not a style preference: a LEFT JOIN's ON clause naming only the preserved
// (outer) side cannot be pushed down into a LATERAL subquery, so Postgres
// evaluates the inner side for every outer row regardless of what the ON
// clause says, and applies it afterward as a plain join filter — the rows come
// back identical either way, so no row-level oracle sees the difference, and
// this review caught the wrong placement twice already (once as a suggestion,
// once as shipped plan text) before it was measured. This is the guard that
// makes a third slip fail here instead of silently reintroducing the cost.
//
// A source scan on honestcounts_test.go's own precedent, for the same reason
// that one gives: the guard's placement is a string in SQL, so only its shape
// can be watched.

import (
	"os"
	"strings"
	"testing"
)

// TestOtherMemberJoinIsSplicedExactlyTwice mirrors
// TestEveryMemberCountUsesTheActivePredicate's own splice count: sync.go has
// exactly two places that resolve a direct's other member —
// loadConversations' page query and conversationRowPerViewer, which CANT-114's
// hub introduction and CANT-75's find-or-create both read — and both have to
// name the one const, or the two SQL sites are independently widened copies
// that can drift, which is the exact bug this ticket's own review named as
// CANT-114's precedent (memberCountExpr and firstUnreadSeqExpr are consts for
// the same reason).
func TestOtherMemberJoinIsSplicedExactlyTwice(t *testing.T) {
	root := storeModuleRoot(t)
	src, err := os.ReadFile(root + "/internal/store/sync.go")
	if err != nil {
		t.Fatalf("read sync.go: %v", err)
	}
	spliced := 0
	for _, line := range strings.Split(string(src), "\n") {
		// Comments name the const too (this file's own header among them), and
		// a comment is not a query. The guard counts the places it is actually
		// concatenated into SQL.
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if strings.Contains(line, "otherMemberJoin") && !strings.HasPrefix(strings.TrimSpace(line), "const otherMemberJoin") {
			spliced++
		}
	}
	if spliced != 2 {
		t.Errorf("sync.go splices otherMemberJoin into %d quer(ies), want 2 — loadConversations' page "+
			"query and conversationRowPerViewer. A direct's other member served from a third, independently "+
			"widened copy is a second place the two could disagree.", spliced)
	}
}

// otherMemberJoinBody returns the raw string literal otherMemberJoin is
// defined as, from its opening backtick to the one that closes it. A const
// declaration, not a func, so funcBody's next-`func`-boundary does not apply;
// the backtick is this literal's own unambiguous end.
func otherMemberJoinBody(t *testing.T, root string) string {
	t.Helper()
	src, err := os.ReadFile(root + "/internal/store/sync.go")
	if err != nil {
		t.Fatalf("read sync.go: %v", err)
	}
	body := string(src)
	const decl = "const otherMemberJoin = `"
	at := strings.Index(body, decl)
	if at < 0 {
		t.Fatal("sync.go: `const otherMemberJoin = `+\"`\"+`` is gone; this guard now watches nothing")
	}
	body = body[at+len(decl):]
	end := strings.Index(body, "`")
	if end < 0 {
		t.Fatal("sync.go: otherMemberJoin's raw string literal is never closed")
	}
	return body[:end]
}

// TestOtherMemberJoinGuardsInsideTheLateralNotOnTheJoin pins the placement
// itself, not only the fragment's presence. Measured (EXPLAIN ANALYZE, a
// clean TEMP-table fixture of 40 groups of 5 members and 10 directs of 2,
// viewer a member of all 50, ANALYZEd): a guard placed `) om ON c.kind =
// 'direct'` ran the inner Hash Join and index scans with loops=50 — every
// conversation row, filtered afterward — while the guard placed inside the
// LATERAL's own WHERE, with `ON true`, showed a One-Time Filter and ran the
// inner join with loops=10 — only the direct rows. A group conversation
// evaluates the subquery at all under the first shape and never does under
// the second, though both return identical rows.
func TestOtherMemberJoinGuardsInsideTheLateralNotOnTheJoin(t *testing.T) {
	root := storeModuleRoot(t)
	body := otherMemberJoinBody(t, root)

	if !strings.Contains(body, "WHERE c.kind = 'direct'") {
		t.Errorf("otherMemberJoin does not contain \"WHERE c.kind = 'direct'\" inside the LATERAL's own "+
			"WHERE clause:\n\n%s", body)
	}
	if !strings.Contains(body, ") om ON true") {
		t.Errorf("otherMemberJoin does not end \") om ON true\" — the LEFT JOIN's own ON clause must carry "+
			"no predicate, or the guard inside WHERE stops being what decides evaluation:\n\n%s", body)
	}
	if strings.Contains(body, "ON c.kind") {
		t.Errorf("otherMemberJoin names \"ON c.kind\" — the guard has moved back onto the LEFT JOIN's own ON "+
			"clause, which cannot push down into a LATERAL and runs the subquery on every row instead of only "+
			"the directs, though the served rows are identical either way:\n\n%s", body)
	}
}
