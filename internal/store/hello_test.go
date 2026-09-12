package store

// CANT-24 / CANT-101 — the hello outcome and its one log line.
//
// Four outcomes, one line each, and the line is the point: it is the only
// measurement of reconnect gaps the deployment has, so each case asserts the
// line's shape and not only the returned outcome.

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestHelloOutcomesAndTheirOneLogLine(t *testing.T) {
	ctx, pool := freshDB(t)
	logger, buf := captureLogger()
	st := New(pool, DefaultLimits(), logger)
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)
	for i := 0; i < 3; i++ {
		send(ctx, t, st, conv, author, "m")
	}
	h := head(ctx, t, pool)
	if h < 3 {
		t.Fatalf("head = %d after three sends, want >= 3", h)
	}
	device, session := uuid.New(), uuid.New()

	cursor := func(v int64) *int64 { return &v }
	cases := []struct {
		name    string
		cursor  *int64
		outcome HelloOutcome
		delta   int64
		level   string
	}{
		{"no cursor", nil, HelloNoCursor, 0, "INFO"},
		{"behind", cursor(h - 2), HelloBehind, 2, "INFO"},
		{"at head", cursor(h), HelloAtHead, 0, "INFO"},
		{"cursor ahead", cursor(h + 7), HelloCursorAhead, -7, "WARN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			res, err := st.Hello(ctx, HelloRequest{DeviceID: device, SessionID: session, Cursor: tc.cursor})
			if err != nil {
				t.Fatalf("hello: %v", err)
			}
			if res.Outcome != tc.outcome {
				t.Errorf("outcome = %q, want %q", res.Outcome, tc.outcome)
			}
			if res.Head != h {
				t.Errorf("head = %d, want %d", res.Head, h)
			}
			if res.Delta != tc.delta {
				t.Errorf("delta = %d, want %d", res.Delta, tc.delta)
			}
			if (res.Cursor == nil) != (tc.cursor == nil) {
				t.Errorf("cursor echoed = %v, want presence %v", res.Cursor, tc.cursor != nil)
			}

			lines := logLines(t, buf)
			if len(lines) != 1 {
				t.Fatalf("got %d log lines, want exactly 1:\n%s", len(lines), buf.String())
			}
			line := lines[0]
			if line["level"] != tc.level {
				t.Errorf("level = %v, want %s", line["level"], tc.level)
			}
			if line["msg"] != "hello" {
				t.Errorf("msg = %v, want hello", line["msg"])
			}
			if line["outcome"] != string(tc.outcome) {
				t.Errorf("outcome attr = %v, want %s", line["outcome"], tc.outcome)
			}
			if line["device_id"] != device.String() || line["session_id"] != session.String() {
				t.Errorf("ids = %v / %v, want %s / %s", line["device_id"], line["session_id"], device, session)
			}
			if got, _ := line["head"].(float64); int64(got) != h {
				t.Errorf("head attr = %v, want %d", line["head"], h)
			}
			_, hasCursor := line["cursor"]
			_, hasDelta := line["delta"]
			if tc.cursor == nil {
				if hasCursor || hasDelta {
					t.Errorf("no-cursor line must carry neither cursor nor delta: %v", line)
				}
			} else {
				if !hasCursor || !hasDelta {
					t.Fatalf("line must carry cursor and delta: %v", line)
				}
				if got, _ := line["delta"].(float64); int64(got) != tc.delta {
					t.Errorf("delta attr = %v, want %d", line["delta"], tc.delta)
				}
			}
		})
	}
}

// The comparison is a read and nothing else: it draws no ordinal, so two
// hellos in a row see the same head, and a hello does not move the counter
// that every other reader's cursor is measured against.
func TestHelloDrawsNothing(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	before := head(ctx, t, pool)
	c := before
	for i := 0; i < 3; i++ {
		if _, err := st.Hello(ctx, HelloRequest{DeviceID: uuid.New(), SessionID: uuid.New(), Cursor: &c}); err != nil {
			t.Fatalf("hello: %v", err)
		}
	}
	if after := head(ctx, t, pool); after != before {
		t.Fatalf("head moved from %d to %d across three hellos; a hello must not draw", before, after)
	}
}

// Delta is Head - Cursor and nothing subtler: a cursor of 0 on a non-empty
// log is `behind` by exactly head, which is the bootstrap-shaped reconnect a
// client that lost its store sends.
func TestHelloZeroCursorIsBehindByHead(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)
	send(ctx, t, st, conv, author, "m")
	h := head(ctx, t, pool)
	var zero int64
	res, err := st.Hello(context.Background(), HelloRequest{DeviceID: uuid.New(), SessionID: uuid.New(), Cursor: &zero})
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	if res.Outcome != HelloBehind || res.Delta != h {
		t.Fatalf("cursor 0: outcome=%s delta=%d, want behind by %d", res.Outcome, res.Delta, h)
	}
}
