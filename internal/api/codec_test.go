package api

// CANT-22 — the codec, held to the vectors.
//
// server/cmd/conformance already proves the GENERATED TYPES agree with the
// TypeScript and Dart clients on every vector. This proves something narrower
// and closer to home: that the DOOR — readFrame, writeFrame and encodeFrame,
// the functions a session actually runs — carries those types over a real
// socket without adding a mistake of its own. A wrong message type, a frame
// split in two, an encoder that reached for the struct instead of the
// generated MarshalJSON: none of those is a type error and none would fail the
// runner.
//
// Every vector goes over the socket. Client frames go through readFrame on the
// server and come back re-encoded; server frames go through writeFrame; the
// non-frame kinds — Message, SyncResponse, the credential payloads — go
// through the same encoder as raw bytes, so the count here is the count in
// the file and nothing is exercised by assumption.
//
// The server is the trust boundary, so `tolerate` is `reject` here — exactly
// as the Go runner says (CANT-74).

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"

	"github.com/magos/catenary/internal/wire"
)

const vectorsPath = "../../schema/vectors/vectors.json"

type vector struct {
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Expect  string          `json:"expect"`
	JSON    json.RawMessage `json:"json"`
	Encoded json.RawMessage `json:"encoded"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(vectorsPath))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var doc struct {
		Cases []vector `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("no vectors: the file moved, not the contract")
	}
	return doc.Cases
}

// canonical sorts keys recursively and keeps large integers exact, so the
// comparison is about values and not field order — the same normalisation the
// three runners use.
func canonical(t *testing.T, raw []byte) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("canonical: %v\n%s", err, raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// outcome is what the loopback server writes back when there is no frame to
// write back: the decoder refused, or ignored.
type outcome struct {
	Result string `json:"__result"`
	Error  string `json:"error,omitempty"`
}

func marshalOutcome(o outcome) []byte {
	b, _ := json.Marshal(o)
	return b
}

func TestTheSocketCodecHoldsEveryVector(t *testing.T) {
	cases := loadVectors(t)

	// The kind of the vector in flight. Set by the client before it writes,
	// read by the server after it reads; the socket orders the two.
	var kind atomic.Pointer[string]

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(1 << 20)
		ctx := r.Context()

		for {
			typ, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var out []byte
			switch k := *kind.Load(); k {
			case "ClientFrame":
				// THE DOOR'S INBOUND PATH, as readFrame runs it.
				f, err := decodeInbound(typ, raw)
				var mf *malformedFrame
				switch {
				case errors.As(err, &mf):
					out = marshalOutcome(outcome{Result: "rejected", Error: mf.Error()})
				case err != nil:
					return
				case f == nil:
					out = marshalOutcome(outcome{Result: "ignored"})
				default:
					if out, err = encodeFrame(f); err != nil {
						out = marshalOutcome(outcome{Result: "encode failed", Error: err.Error()})
					}
				}
			default:
				var v any
				if k == "ServerFrame" {
					// Decoded with the generated decoder a CLIENT would use,
					// then sent through THE DOOR'S OUTBOUND PATH.
					var sf wire.ServerFrame
					if sf, err = wire.DecodeServerFrame(raw); err == nil && sf != nil {
						if err := writeFrame(ctx, conn, sf); err != nil {
							return
						}
						continue
					}
					v = sf
				} else {
					v, err = wire.DecodeNamed(k, raw)
				}
				switch {
				case err != nil:
					out = marshalOutcome(outcome{Result: "rejected", Error: err.Error()})
				case v == nil:
					out = marshalOutcome(outcome{Result: "ignored"})
				default:
					if out, err = encodeFrame(v); err != nil {
						out = marshalOutcome(outcome{Result: "encode failed", Error: err.Error()})
					}
				}
			}
			if err := conn.Write(ctx, websocket.MessageText, out); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.Dial(testCtx(t), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	ran := 0
	for _, c := range cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			ran++
			kind.Store(&c.Kind)
			if err := conn.Write(testCtx(t), websocket.MessageText, c.JSON); err != nil {
				t.Fatal(err)
			}
			typ, reply, err := conn.Read(testCtx(t))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if typ != websocket.MessageText {
				t.Fatalf("the door wrote a %v frame; the wire is text", typ)
			}

			var o outcome
			_ = json.Unmarshal(reply, &o)
			switch c.Expect {
			case "reject", "tolerate":
				if o.Result != "rejected" {
					t.Errorf("the server decoded a %s the schema %ss: %s", c.Kind, c.Expect, reply)
				}
			case "ignore":
				if o.Result != "ignored" {
					t.Errorf("an unknown tag was not ignored: %s", reply)
				}
			case "roundtrip":
				if o.Result != "" {
					t.Fatalf("%s: %s", o.Result, o.Error)
				}
				want := c.JSON
				if len(c.Encoded) > 0 {
					want = c.Encoded
				}
				if got, exp := canonical(t, reply), canonical(t, want); got != exp {
					t.Errorf("\n want %s\n got  %s", exp, got)
				}
			default:
				t.Fatalf("unknown expectation %q", c.Expect)
			}
		})
	}
	if ran != len(cases) {
		t.Fatalf("ran %d of %d vectors", ran, len(cases))
	}
	t.Logf("all %d vectors carried over the socket, none skipped", ran)
}
