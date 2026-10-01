package main

// CANT-177 row 0 (CANT-180): the negative control for the client pass.
//
// The client pass is only worth having if it can tell the two sides apart. So
// it is run here with the STRICT decoder in place of the client one, and the
// cases it fails must be exactly the `tolerate` cases, by name: fewer would
// mean a `tolerate` case the client side does not actually tolerate, and more
// would mean the client side differs from the server somewhere it must not.

import (
	"io"
	"path/filepath"
	"slices"
	"testing"

	"github.com/magos/catenary/internal/wire"
)

func loadVectors(t *testing.T) []testCase {
	t.Helper()
	cases, err := loadCases(filepath.Join("..", "..", "..", "schema", "vectors", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestTheClientPassWithTheStrictDecoderFailsExactlyTheToleratedCases(t *testing.T) {
	cases := loadVectors(t)

	var tolerate []string
	for _, c := range cases {
		if c.Expect == "tolerate" {
			tolerate = append(tolerate, c.Name)
		}
	}
	// An empty set would make the comparison below vacuous.
	if len(tolerate) == 0 {
		t.Fatal("no tolerate cases in the vectors; this control proves nothing without them")
	}

	strictAsClient := clientSide
	strictAsClient.decode = wire.DecodeNamed
	failed := runPass(cases, strictAsClient, io.Discard)

	slices.Sort(failed)
	slices.Sort(tolerate)
	if !slices.Equal(failed, tolerate) {
		t.Fatalf("client pass with the strict decoder failed\n  %q\nwant exactly the tolerate cases\n  %q", failed, tolerate)
	}
	for _, name := range failed {
		t.Logf("fails as it must: %s", name)
	}
}

func TestBothPassesAreGreen(t *testing.T) {
	wire.SetOnUnknownWireValue(func(string) {})
	t.Cleanup(func() { wire.SetOnUnknownWireValue(nil) })

	cases := loadVectors(t)
	for _, s := range []side{serverSide, clientSide} {
		if failed := runPass(cases, s, io.Discard); len(failed) != 0 {
			t.Errorf("%s pass failed %q", s.name, failed)
		}
	}
}
