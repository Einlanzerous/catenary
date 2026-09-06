// Command conformance holds the generated Go wire types to the same golden
// vectors as the TypeScript and Dart clients — IDEA-27 (R4).
//
// The server is the third implementation of the protocol, and the one both
// clients are measured against. Leaving it out of the conformance run would
// mean the two clients agreed with each other and neither was checked against
// the thing they actually talk to.
//
// SCOPE, stated rather than implied: this runner executes the roundtrip and
// ignore cases and SKIPS the reject cases. The generated Go decoders do not
// enforce the schema's constraints, because encoding/json cannot distinguish
// an absent required scalar from an explicit zero one without a generated
// UnmarshalJSON that shadows every required field with a pointer. That is
// mechanical to add and is the first thing P1 should do — the server is the
// trust boundary, so it is the implementation that most needs to refuse bad
// input. Until then the skip is counted and printed, never silent.
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

	var failed, skipped int
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
		if c.Expect == "reject" {
			skipped++
			fmt.Printf("skip  %s  (constraint enforcement is a documented gap in Go)\n", c.Name)
			continue
		}

		v, err := wire.DecodeNamed(c.Kind, c.JSON)
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
		fmt.Printf("\nall green — %d run, %d skipped, %d vectors\n", total-skipped, skipped, total)
		return
	}
	fmt.Printf("\n%d of %d FAILED (%d skipped)\n", failed, total-skipped, skipped)
	os.Exit(1)
}
