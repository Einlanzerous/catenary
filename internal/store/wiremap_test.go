package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Criterion 12 — the staleness guard, made falsifiable
//
// Rev 2 of the plan asked for a guard that "fails CI when the generated schema
// and the migrations disagree, proved by making them disagree". The wire
// schema and the DDL have no mechanical relation, so "disagree" was undefined:
// there is nothing to regenerate and diff. A criterion that cannot be met is
// not failed at verification time, it is reinterpreted.
//
// So the relation is written down instead — schema/mapping/wire-fields.json,
// every wire field against column / derived / client-local — and this guard
// fails on:
//
//   - a wire field with no mapping, or a mapping naming a field the wire does
//     not have (no database needed, so it runs everywhere);
//   - a column mapping pointing at a column that does not exist;
//   - a column whose type contradicts the wire type;
//   - a REQUIRED wire field backed by a nullable column that says nothing about
//     why — either the CHECK that makes it present anyway, or the reason both
//     are correct.
//
// "Make them disagree" is then real: add a field to the wire schema and watch
// this go red.

const (
	wireSchemaPath = "../../schema/catenary.wire.v1.schema.json"
	wireMapPath    = "../../schema/mapping/wire-fields.json"
)

type wireMapping struct {
	Kind            string `json:"kind"`
	Table           string `json:"table"`
	Column          string `json:"column"`
	JSONPath        string `json:"json_path"`
	RequiredVia     string `json:"required_via"`
	NullableBecause string `json:"nullable_because"`
	Note            string `json:"note"`
}

type wireMap struct {
	Fields map[string]wireMapping `json:"fields"`
}

// wireField is one field of the wire schema, with its type already resolved
// through any $ref.
type wireField struct {
	name     string // "Message.seq"
	kind     string // uuid | timestamp | string | integer | boolean | array | object
	required bool
}

func loadWireFields(t *testing.T) []wireField {
	t.Helper()
	raw, err := os.ReadFile(wireSchemaPath)
	if err != nil {
		t.Fatalf("read wire schema: %v", err)
	}
	var doc struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse wire schema: %v", err)
	}

	nodes := map[string]map[string]any{}
	for name, rawNode := range doc.Defs {
		var n map[string]any
		if err := json.Unmarshal(rawNode, &n); err != nil {
			t.Fatalf("parse $defs/%s: %v", name, err)
		}
		nodes[name] = n
	}

	var out []wireField
	for name, node := range nodes {
		if node["type"] != "object" {
			continue
		}
		props, _ := node["properties"].(map[string]any)
		var required []string
		if r, ok := node["required"].([]any); ok {
			for _, v := range r {
				required = append(required, v.(string))
			}
		}
		for prop, rawProp := range props {
			p, _ := rawProp.(map[string]any)
			out = append(out, wireField{
				name:     name + "." + prop,
				kind:     resolveKind(t, nodes, p, name+"."+prop, 0),
				required: slices.Contains(required, prop),
			})
		}
	}
	slices.SortFunc(out, func(a, b wireField) int { return strings.Compare(a.name, b.name) })
	return out
}

// resolveKind reduces one property schema to the handful of shapes a column
// can back. Anything it does not understand is a hard failure rather than a
// shrug: a guard that silently skips what it cannot parse is a guard that
// stops covering the schema one keyword at a time.
func resolveKind(t *testing.T, nodes map[string]map[string]any, p map[string]any, field string, depth int) string {
	t.Helper()
	if depth > 8 {
		t.Fatalf("%s: $ref cycle", field)
	}
	if ref, ok := p["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/$defs/")
		target, ok := nodes[name]
		if !ok {
			t.Fatalf("%s: $ref %s resolves to nothing", field, ref)
		}
		return resolveKind(t, nodes, target, field, depth+1)
	}
	if _, ok := p["enum"]; ok {
		return "string"
	}
	if c, ok := p["const"]; ok {
		if _, isStr := c.(string); isStr {
			return "string"
		}
		t.Fatalf("%s: non-string const %v", field, c)
	}
	if _, ok := p["oneOf"]; ok {
		return "object"
	}
	switch p["type"] {
	case "string":
		switch p["format"] {
		case "uuid":
			return "uuid"
		case "date-time":
			return "timestamp"
		}
		return "string"
	case "integer", "number":
		return "integer"
	case "boolean":
		return "boolean"
	case "array":
		return "array"
	case "object":
		return "object"
	}
	t.Fatalf("%s: cannot resolve a type from %v", field, p)
	return ""
}

func loadWireMap(t *testing.T) wireMap {
	t.Helper()
	raw, err := os.ReadFile(wireMapPath)
	if err != nil {
		t.Fatalf("read wire map: %v", err)
	}
	var m wireMap
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse wire map: %v", err)
	}
	return m
}

// The half that needs no database, so it runs on every machine and in every
// lane. This is the one that catches "somebody added a wire field".
func TestEveryWireFieldIsMapped(t *testing.T) {
	fields := loadWireFields(t)
	m := loadWireMap(t)

	if len(fields) == 0 {
		t.Fatal("parsed no fields out of the wire schema")
	}

	seen := map[string]bool{}
	for _, f := range fields {
		seen[f.name] = true
		e, ok := m.Fields[f.name]
		if !ok {
			t.Errorf("%s is on the wire and not in %s — decide whether it is a column, derived, or client-local", f.name, wireMapPath)
			continue
		}
		switch e.Kind {
		case "column":
			if e.Table == "" || e.Column == "" {
				t.Errorf("%s is mapped as a column with no table/column", f.name)
			}
		case "derived", "client-local":
			// The reason is the whole value of the entry: "derived" with no
			// note records that somebody typed a word, not that they decided.
			if strings.TrimSpace(e.Note) == "" {
				t.Errorf("%s is %q with no note saying what it is derived from or why the server has no opinion", f.name, e.Kind)
			}
		default:
			t.Errorf("%s has kind %q, want column, derived or client-local", f.name, e.Kind)
		}
	}

	for name := range m.Fields {
		if !seen[name] {
			t.Errorf("%s is mapped and is not on the wire — a stale entry outlives the field it described", name)
		}
	}
}

// The half that needs the database. A mapping is only worth having if it is
// checked against the columns it claims.
func TestWireMapMatchesTheDatabase(t *testing.T) {
	ctx, pool := freshDB(t)
	fields := loadWireFields(t)
	m := loadWireMap(t)

	cols := loadColumns(ctx, t, pool)
	checks := loadCheckConstraints(ctx, t, pool)

	// A wire kind against the Postgres types that can honestly carry it.
	compatible := map[string][]string{
		"uuid":      {"uuid"},
		"timestamp": {"timestamp with time zone"},
		"string":    {"text"},
		"integer":   {"smallint", "integer", "bigint"},
		"boolean":   {"boolean"},
		"array":     {"ARRAY"},
		"object":    {"jsonb"},
	}

	for _, f := range fields {
		e, ok := m.Fields[f.name]
		if !ok || e.Kind != "column" {
			continue
		}
		key := e.Table + "." + e.Column
		c, ok := cols[key]
		if !ok {
			t.Errorf("%s maps to %s, which does not exist", f.name, key)
			continue
		}

		// A field carried inside a JSONB document is checked as far as it can
		// honestly be checked: the backing column must be a document. What is
		// inside it is the document's business, and saying so beats a type
		// check that pretends to more than it does.
		if e.JSONPath != "" {
			if c.dataType != "jsonb" {
				t.Errorf("%s names a json_path but %s is %s, not jsonb", f.name, key, c.dataType)
			}
			continue
		}

		if !slices.Contains(compatible[f.kind], c.dataType) {
			t.Errorf("%s is %s on the wire and %s in %s — the two contradict", f.name, f.kind, c.dataType, key)
		}

		// A required wire field backed by a nullable column has to say why.
		// Both legitimate shapes exist here: one attachments table carrying two
		// kinds (a CHECK makes the field present for its own kind), and a field
		// required inside an OPTIONAL parent. What is not legitimate is
		// silence, because that is indistinguishable from an oversight — and
		// this guard found four of these the first time it ran, one of which
		// neither plan revision had named.
		if f.required && c.nullable {
			switch {
			case e.RequiredVia != "":
				if !slices.Contains(checks[e.Table], e.RequiredVia) {
					t.Errorf("%s names required_via %q, which is not a check constraint on %s", f.name, e.RequiredVia, e.Table)
				}
			case strings.TrimSpace(e.NullableBecause) != "":
				// Stated and accepted.
			default:
				t.Errorf("%s is required on the wire and %s is nullable, with neither a required_via nor a nullable_because", f.name, key)
			}
		}
	}
}

type dbColumn struct {
	dataType string
	nullable bool
}

func loadColumns(ctx context.Context, t *testing.T, pool *pgxpool.Pool) map[string]dbColumn {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT table_name, column_name, data_type, is_nullable = 'YES'
		FROM information_schema.columns WHERE table_schema = 'public'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	out := map[string]dbColumn{}
	var table, column string
	var c dbColumn
	if _, err := pgx.ForEachRow(rows, []any{&table, &column, &c.dataType, &c.nullable}, func() error {
		out[fmt.Sprintf("%s.%s", table, column)] = c
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func loadCheckConstraints(ctx context.Context, t *testing.T, pool *pgxpool.Pool) map[string][]string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT rel.relname, c.conname
		FROM pg_constraint c
		JOIN pg_class rel ON rel.oid = c.conrelid
		WHERE c.contype = 'c' AND rel.relnamespace = 'public'::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	out := map[string][]string{}
	var table, name string
	if _, err := pgx.ForEachRow(rows, []any{&table, &name}, func() error {
		out[table] = append(out[table], name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
