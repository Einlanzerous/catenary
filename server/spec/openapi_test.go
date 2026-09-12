// Package spec holds the emitted OpenAPI document to the parser it is emitted
// for — CANT-74, closing the step CANT-12's PR #3 re-review left open.
//
// The staleness guard proves schema/openapi.yaml matches the wire schema. It
// does not prove the result is valid OpenAPI 3.0, and until this test nothing
// in the tree did: CANT-12 validated once by hand at Argosy's pin and recorded
// the number in a PR body. This makes that a step that re-runs.
//
// kin-openapi is pinned at v0.135.0 on purpose. It is the version Argosy pins
// and the parser oapi-codegen v2.7.1 goes through, so "loads here" means
// "loads in the house pipeline" rather than "loads in some parser".
//
// TWO NEGATIVE CONTROLS, AND WHAT THEY MEASURED. The same CANT-12 finding
// named a second gap: the emitter's pass-through arm copies any keyword it
// does not know into the spec unrewritten. This test injects each failure mode
// into the real document and records what the parser does with it, so
// CANT-105 (the emitter allow-list) starts from a fact rather than a guess.
// The CANT-74 plan expected the parse to catch one of the two. It catches
// BOTH:
//
//   - an unrewritten `#/$defs/` reference — refused at LOAD: the fragment
//     cannot resolve.
//   - a stray non-`x-` keyword — refused at VALIDATE: kin-openapi treats any
//     key it does not know on a Schema Object as an "extra sibling field".
//
// So a keyword the emitter passes through by mistake fails this test in CI
// with the keyword named. What CANT-105 still buys is the error at EMIT time,
// naming the schema path rather than the parser's view of it, and turning the
// keyword census into an assertion — it is no longer the only thing standing
// between a stray keyword and a green build. Both assertions pin the measured
// behaviour; if a later kin-openapi stops refusing either, the control fails
// and the record on CANT-105 needs updating, which is the point.
package spec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

const specPath = "../../schema/openapi.yaml"

// loadThenValidate reports which stage refused a document, so a control can
// pin not only THAT the parser refuses it but WHERE.
func loadThenValidate(doc []byte) (stage string, err error) {
	loader := openapi3.NewLoader()
	parsed, err := loader.LoadFromData(doc)
	if err != nil {
		return "load", err
	}
	if err := parsed.Validate(context.Background()); err != nil {
		return "validate", err
	}
	return "", nil
}

func loadAndValidate(t *testing.T, doc []byte) error {
	t.Helper()
	_, err := loadThenValidate(doc)
	return err
}

func readSpec(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(specPath))
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	return string(b)
}

// TestEmittedSpecLoadsAndValidates is the committed parse.
func TestEmittedSpecLoadsAndValidates(t *testing.T) {
	src := readSpec(t)
	if err := loadAndValidate(t, []byte(src)); err != nil {
		t.Fatalf("schema/openapi.yaml does not load under kin-openapi v0.135.0: %v", err)
	}
	// The CANT-74 marker is a specification extension and must survive the
	// parse as one: present on a client-open enum, absent on a closed one.
	loader := openapi3.NewLoader()
	parsed, err := loader.LoadFromData([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	open := parsed.Components.Schemas["DeliveryState"].Value.Extensions["x-catenary-client-open"]
	if open != true {
		t.Fatalf("DeliveryState should carry x-catenary-client-open: true, got %#v", open)
	}
	if _, has := parsed.Components.Schemas["TypingState"].Value.Extensions["x-catenary-client-open"]; has {
		t.Fatal("TypingState is closed everywhere and must not carry x-catenary-client-open")
	}
}

// TestNegativeControlUnrewrittenDefsRef: the pass-through arm's first failure
// mode. Measured: refused at load, because the fragment cannot resolve.
func TestNegativeControlUnrewrittenDefsRef(t *testing.T) {
	src := readSpec(t)
	const good = `"$ref": "#/components/schemas/Uuid"`
	if !strings.Contains(src, good) {
		t.Fatalf("control needs %s in the document", good)
	}
	mutated := strings.Replace(src, good, `"$ref": "#/$defs/Uuid"`, 1)
	stage, err := loadThenValidate([]byte(mutated))
	if err == nil {
		t.Fatal("MEASURED CHANGE: kin-openapi now accepts an unrewritten #/$defs/ reference; update the record on CANT-105")
	}
	if stage != "load" {
		t.Fatalf("MEASURED CHANGE: the unrewritten reference was refused at %s rather than at load: %v", stage, err)
	}
	t.Logf("unrewritten #/$defs/ reference: refused at %s — %v", stage, err)
}

// TestNegativeControlStrayKeyword: the pass-through arm's second failure mode.
// Measured: refused at validate, as an "extra sibling field" on the schema
// that reaches it first. The plan expected this one to pass; it does not.
func TestNegativeControlStrayKeyword(t *testing.T) {
	src := readSpec(t)
	const anchor = "    Uuid:\n      type: \"string\"\n"
	if !strings.Contains(src, anchor) {
		t.Fatalf("control needs the Uuid component in the document")
	}
	mutated := strings.Replace(src, anchor, "    Uuid:\n      prefixItems: []\n      type: \"string\"\n", 1)
	stage, err := loadThenValidate([]byte(mutated))
	if err == nil {
		t.Fatal("MEASURED CHANGE: kin-openapi now accepts a stray non-x- keyword; CANT-105 becomes the only guard again and its record needs updating")
	}
	if stage != "validate" || !strings.Contains(err.Error(), "prefixItems") {
		t.Fatalf("MEASURED CHANGE: the stray keyword was refused at %s with a message that does not name it: %v", stage, err)
	}
	t.Logf("stray non-x- keyword (prefixItems): refused at %s — %v", stage, err)
}
