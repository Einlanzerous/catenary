// Command conformance holds the generated Go wire types to the same golden
// vectors as the TypeScript and Dart clients — IDEA-27 (R4).
//
// The server is the third implementation of the protocol, and the one both
// clients are measured against. Leaving it out of the conformance run would
// mean the two clients agreed with each other and neither was checked against
// the thing they actually talk to.
//
// SCOPE: all three kinds — roundtrip, ignore AND reject. Nothing is skipped.
//
// CANT-25 closed the gap this comment used to describe. The generated decoders
// now enforce the schema's constraints, so the ten reject cases run here rather
// than being counted and printed as skips. The mechanism is an UnmarshalJSON
// that decodes into a shadow whose fields are all pointers, which is what makes
// an absent required scalar distinguishable from an explicit zero one —
// something encoding/json cannot do on its own, and the reason a Message with
// no seq used to decode cleanly.
//
// A reject case passes when the decoder REFUSES it, and NOTHING HERE CHECKS
// WHY. `reject_seq_zero` would still pass if the minimum check were dropped and
// some unrelated constraint broke instead.
//
// That is a real gap and it is stated rather than papered over. An earlier
// version of this comment claimed store-side tests assert on the decoder's
// message; they do not — nothing in either module asserts on a DecodeError's
// text. Closing it properly means an expected-message field on the vectors,
// which is a change to the contract all three runners share and wants its own
// ticket rather than riding along here. The refusal IS printed next to each
// case, so a reader sees which constraint fired even though no assertion pins
// it.
//
// Run: go run ./cmd/conformance   (from the server/ directory)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/magos/catenary/internal/wire"
)

type testCase struct {
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Expect  string          `json:"expect"`
	Why     string          `json:"why"`
	JSON    json.RawMessage `json:"json"`
	Encoded json.RawMessage `json:"encoded"`
}

// canonical re-encodes JSON with object keys sorted recursively, so key order
// is not asserted and everything else is. UseNumber keeps large integers exact
// — without it 9007199254740991 round-trips through float64 and comes back in
// scientific notation, which would fail the boundary vector for the wrong
// reason.
func canonical(raw []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	// encoding/json sorts map[string]any keys on marshal, which is the whole
	// normalisation we need.
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func main() {
	path := filepath.Join("..", "schema", "vectors", "vectors.json")
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: %v\n", err)
		os.Exit(2)
	}

	var doc struct {
		Cases []testCase `json:"cases"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		fmt.Fprintf(os.Stderr, "conformance: %v\n", err)
		os.Exit(2)
	}

	var failed int
	check := func(name string, ok bool, detail string) {
		if !ok {
			failed++
		}
		status := "ok  "
		if !ok {
			status = "FAIL"
		}
		if detail != "" {
			fmt.Printf("%s  %s  (%s)\n", status, name, detail)
			return
		}
		fmt.Printf("%s  %s\n", status, name)
	}

	for _, c := range doc.Cases {
		v, err := wire.DecodeNamed(c.Kind, c.JSON)

		// A reject case is the one where an error is the PASS. The detail
		// carries the decoder's own message, so a reader sees which constraint
		// fired rather than only that one did.
		if c.Expect == "reject" {
			if err != nil {
				check(c.Name, true, "refused: "+err.Error())
			} else {
				check(c.Name, false, "DECODED a frame the schema rejects")
			}
			continue
		}

		if err != nil {
			check(c.Name, false, fmt.Sprintf("decode: %v", err))
			continue
		}

		if c.Expect == "ignore" {
			check(c.Name, v == nil, map[bool]string{true: "ignored", false: "decoded an unknown tag"}[v == nil])
			continue
		}

		if v == nil {
			check(c.Name, false, "decoder returned nil for a known tag")
			continue
		}

		got, err := json.Marshal(v)
		if err != nil {
			check(c.Name, false, fmt.Sprintf("encode: %v", err))
			continue
		}

		expectRaw := c.JSON
		if len(c.Encoded) > 0 {
			expectRaw = c.Encoded
		}
		want, err := canonical(expectRaw)
		if err != nil {
			check(c.Name, false, fmt.Sprintf("canonical(want): %v", err))
			continue
		}
		gotC, err := canonical(got)
		if err != nil {
			check(c.Name, false, fmt.Sprintf("canonical(got): %v", err))
			continue
		}
		if want == gotC {
			check(c.Name, true, "")
		} else {
			check(c.Name, false, fmt.Sprintf("\n    want %s\n    got  %s", want, gotC))
		}
	}

	total := len(doc.Cases)
	if failed == 0 {
		fmt.Printf("\nall green — %d vectors, none skipped\n", total)
		return
	}
	fmt.Printf("\n%d of %d FAILED\n", failed, total)
	os.Exit(1)
}
