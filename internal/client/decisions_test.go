package client

// CANT-156: the shared decision vectors, from the Go side.
//
// CANT-31's rules are implemented three times — here, in web/src/transport and
// in CANT-42's Dart — and two of them disagreeing is a device that stops when it
// should reconnect, or one that keeps minting refresh links on a dead network.
// Neither is a type error, so codegen cannot catch it. testdata/decisions.json
// is the answer CANT-35 ruling 6 picked: ONE vector file that every language's
// tests read, instead of each language hand-porting this package's tables. Its
// note is the contract; web/decisions.ts is the TypeScript runner over the same
// file.
//
// EACH KIND DISPATCHES TO EXACTLY ONE FUNCTION, and this runner implements no
// rule of its own. The pure kinds call the package-level cores the methods
// delegate to (classifyClose, refreshThreshold, refreshDue, refreshDelay,
// readStamp, gateOpen, refreshHoldAt, refusedHoldAt, and CANT-170's resetsRamp,
// unitFromBytes, jitteredWait and advance). The chain kind drives the
// client's own RefreshIfDue against a scripted /refresh that answers each
// request verbatim from the vector — not `family`, whose model of the server
// would otherwise have to be hand-written again in TypeScript and in Dart.
//
// A RUNNER THAT CANNOT FAIL PROVES NOTHING, so TestTheDecisionVectorsHaveTeeth
// swaps each function for a mutant wrong at exactly one boundary, runs the chain
// transcripts under the two chain faults, and requires every one of them to fail
// a named case.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

const decisionsFile = "testdata/decisions.json"

// decisionKinds is every kind the file may hold. A case of any other kind fails,
// and so does a kind here with no cases: a kind nobody wrote a vector for pins
// nothing, and would read as coverage.
var decisionKinds = []string{
	"close", "threshold", "due", "delay", "stamp", "gate", "hold", "refused_hold", "chain",
	"backoff_reset", "backoff_draw", "backoff_jitter", "backoff_advance",
}

type decisionCase struct {
	Name string          `json:"name"`
	Kind string          `json:"kind"`
	Why  string          `json:"why"`
	In   json.RawMessage `json:"in"`
	Want json.RawMessage `json:"want"`
}

// loadDecisions reads the file and refuses a malformed one: no note, a case
// without a name, a why, an in or a want, or two cases with one name.
func loadDecisions(t *testing.T) []decisionCase {
	t.Helper()
	raw, err := os.ReadFile(decisionsFile)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Note  string         `json:"note"`
		Cases []decisionCase `json:"cases"`
	}
	if err := strict(raw, &file); err != nil {
		t.Fatalf("%s: %v", decisionsFile, err)
	}
	if file.Note == "" || len(file.Cases) == 0 {
		t.Fatalf("%s: no note, or no cases", decisionsFile)
	}
	seen := map[string]bool{}
	for i, c := range file.Cases {
		if c.Name == "" || strings.TrimSpace(c.Why) == "" || len(c.In) == 0 || len(c.Want) == 0 {
			t.Fatalf("%s: case %d (%q) needs a name, a why, an in and a want", decisionsFile, i, c.Name)
		}
		if seen[c.Name] {
			t.Fatalf("%s: two cases are named %q", decisionsFile, c.Name)
		}
		seen[c.Name] = true
	}
	return file.Cases
}

// decisions is the one function each kind dispatches to. reference is this
// package's own; the teeth test replaces one field at a time.
type decisions struct {
	close     func(websocket.StatusCode, *wire.ServerError) closeVerdict
	threshold func(Credential) time.Duration
	due       func(Credential, time.Time) bool
	delay     func(links int) time.Duration
	stamp     func(lastSent, now time.Time) time.Time
	gate      func(answeredAt, stamp time.Time) bool
	hold      func(links int, answeredAt, lastSent, now time.Time) RefreshHold
	refused   func(refused bool, links int, lastSent, now time.Time) bool
	// The dial backoff's pure pieces (CANT-170).
	reset   func(readied bool, readiedFor, interval time.Duration) bool
	draw    func([4]byte) float64
	jitter  func(nominal time.Duration, unit float64) time.Duration
	advance func(nominal, ceiling time.Duration) time.Duration
	// faults is what the chain transcripts' client runs with.
	faults Faults
	// decode is what a close case's `preceding` goes through: the decoder the
	// session loop uses (CANT-177), never a hand-built frame.
	decode func([]byte) (wire.ServerFrame, error)
}

var reference = decisions{
	close: func(s websocket.StatusCode, p *wire.ServerError) closeVerdict {
		v, _ := classifyClose(s, p)
		return v
	},
	threshold: refreshThreshold,
	due:       refreshDue,
	delay:     refreshDelay,
	stamp:     readStamp,
	gate:      gateOpen,
	hold:      refreshHoldAt,
	refused:   refusedHoldAt,
	reset:     resetsRamp,
	draw:      unitFromBytes,
	jitter:    jitteredWait,
	advance:   advance,
	decode:    wire.DecodeServerFrameAsClient,
}

// The file spells answers as the TypeScript that shipped does; this is the one
// table from Go's constants to those spellings.
var (
	verdictNames = map[closeVerdict]string{reconnect: "reconnect", reconnectAtMaximum: "reconnect_at_maximum", stopProtocolFailure: "terminal_protocol"}
	holdNames    = map[RefreshHold]string{RefreshNotHeld: "none", RefreshHeldUnreachable: "unreachable", RefreshHeldBackoff: "backoff"}
)

// run answers one case, and says what disagreed.
func (d decisions) run(c decisionCase) error {
	switch c.Kind {
	case "close":
		var in struct {
			Status    *int            `json:"status"`
			Preceding json.RawMessage `json:"preceding"`
		}
		var want struct {
			Verdict *string `json:"verdict"`
		}
		if err := parseCase(c, &in, &want); err != nil {
			return err
		}
		status := websocket.StatusCode(-1) // no close frame, as coder/websocket reports it
		if in.Status != nil {
			status = websocket.StatusCode(*in.Status)
		}
		preceding, err := decodePreceding(d.decode, in.Preceding)
		if err != nil {
			return err
		}
		return same("verdict", verdictNames[d.close(status, preceding)], want.Verdict)

	case "threshold":
		var in struct {
			IssuedAt  *string `json:"access_issued_at"`
			ExpiresAt *string `json:"access_expires_at"`
		}
		var want struct {
			ThresholdMs *int64 `json:"threshold_ms"`
		}
		if err := parseCase(c, &in, &want); err != nil {
			return err
		}
		var cr Credential
		if err := instants(&cr.AccessIssuedAt, in.IssuedAt, &cr.AccessExpiresAt, in.ExpiresAt); err != nil {
			return err
		}
		if want.ThresholdMs == nil {
			return errors.New("want.threshold_ms is missing")
		}
		if got, w := d.threshold(cr), time.Duration(*want.ThresholdMs)*time.Millisecond; got != w {
			return fmt.Errorf("threshold %s, want %s", got, w)
		}
		return nil

	case "due":
		var in struct {
			IssuedAt      *string `json:"access_issued_at"`
			ExpiresAt     *string `json:"access_expires_at"`
			ClockOffsetMs int64   `json:"clock_offset_ms"`
			DeviceNow     *string `json:"device_now"`
		}
		var want struct {
			Due *bool `json:"due"`
		}
		if err := parseCase(c, &in, &want); err != nil {
			return err
		}
		cr := Credential{ClockOffset: time.Duration(in.ClockOffsetMs) * time.Millisecond}
		var now time.Time
		if err := instants(&cr.AccessIssuedAt, in.IssuedAt, &cr.AccessExpiresAt, in.ExpiresAt, &now, in.DeviceNow); err != nil {
			return err
		}
		return same("due", d.due(cr, now), want.Due)

	case "delay":
		var in struct {
			Links int `json:"links"`
		}
		var want struct {
			DelayMs *int64 `json:"delay_ms"`
		}
		if err := parseCase(c, &in, &want); err != nil {
			return err
		}
		if want.DelayMs == nil {
			return errors.New("want.delay_ms is missing")
		}
		if got, w := d.delay(in.Links), time.Duration(*want.DelayMs)*time.Millisecond; got != w {
			return fmt.Errorf("%d links wait %s, want %s", in.Links, got, w)
		}
		return nil

	case "stamp":
		var in struct {
			LastSent *string `json:"last_sent"`
			Now      *string `json:"now"`
		}
		var want struct {
			Stamp *string `json:"stamp"`
		}
		if err := parseCase(c, &in, &want); err != nil {
			return err
		}
		var sent, now, w time.Time
		if err := instants(&sent, in.LastSent, &now, in.Now, &w, want.Stamp); err != nil {
			return err
		}
		if got := d.stamp(sent, now); !got.Equal(w) {
			return fmt.Errorf("stamp %s, want %s", showInstant(got), showInstant(w))
		}
		return nil

	case "gate":
		var in struct {
			AnsweredAt *string `json:"answered_at"`
			Stamp      *string `json:"stamp"`
		}
		var want struct {
			Open *bool `json:"open"`
		}
		if err := parseCase(c, &in, &want); err != nil {
			return err
		}
		var answered, stamp time.Time
		if err := instants(&answered, in.AnsweredAt, &stamp, in.Stamp); err != nil {
			return err
		}
		return same("open", d.gate(answered, stamp), want.Open)

	case "hold":
		var in struct {
			Links      int     `json:"links"`
			AnsweredAt *string `json:"answered_at"`
			LastSent   *string `json:"last_sent"`
			Now        *string `json:"now"`
		}
		var want struct {
			Hold *string `json:"hold"`
		}
		if err := parseCase(c, &in, &want); err != nil {
			return err
		}
		var answered, sent, now time.Time
		if err := instants(&answered, in.AnsweredAt, &sent, in.LastSent, &now, in.Now); err != nil {
			return err
		}
		return same("hold", holdNames[d.hold(in.Links, answered, sent, now)], want.Hold)

	case "refused_hold":
		var in struct {
			Refused  bool    `json:"refused"`
			Links    int     `json:"links"`
			LastSent *string `json:"last_sent"`
			Now      *string `json:"now"`
		}
		var want struct {
			Hold *bool `json:"hold"`
		}
		if err := parseCase(c, &in, &want); err != nil {
			return err
		}
		var sent, now time.Time
		if err := instants(&sent, in.LastSent, &now, in.Now); err != nil {
			return err
		}
		return same("hold", d.refused(in.Refused, in.Links, sent, now), want.Hold)

	case "chain":
		return d.chain(c)

	case "backoff_reset":
		var in struct {
			ReadiedForMs *int64 `json:"readied_for_ms"`
			IntervalSec  *int64 `json:"heartbeat_interval_sec"`
		}
		var want struct {
			Resets *bool `json:"resets"`
		}
		if err := parseCase(c, &in, &want); err != nil {
			return err
		}
		if in.IntervalSec == nil {
			return errors.New("in.heartbeat_interval_sec is missing: a session that readied always has one")
		}
		var readiedFor time.Duration
		if in.ReadiedForMs != nil {
			readiedFor = time.Duration(*in.ReadiedForMs) * time.Millisecond
		}
		return same("resets", d.reset(in.ReadiedForMs != nil, readiedFor, time.Duration(*in.IntervalSec)*time.Second), want.Resets)

	case "backoff_draw":
		var in struct {
			Bytes string `json:"bytes"`
		}
		var want struct {
			Unit *float64 `json:"unit"`
		}
		if err := parseCase(c, &in, &want); err != nil {
			return err
		}
		raw, err := hex.DecodeString(in.Bytes)
		if err != nil || len(raw) != 4 {
			return fmt.Errorf("in.bytes %q is not four bytes of hex", in.Bytes)
		}
		return same("unit", d.draw([4]byte(raw)), want.Unit)

	case "backoff_jitter":
		var in struct {
			NominalMs int64   `json:"nominal_ms"`
			Unit      float64 `json:"unit"`
		}
		var want struct {
			WaitMs *float64 `json:"wait_ms"`
		}
		if err := parseCase(c, &in, &want); err != nil {
			return err
		}
		if want.WaitMs == nil {
			return errors.New("want.wait_ms is missing")
		}
		w := time.Duration(math.Round(*want.WaitMs * float64(time.Millisecond)))
		if got := d.jitter(time.Duration(in.NominalMs)*time.Millisecond, in.Unit); got != w {
			return fmt.Errorf("%d ms at draw %v waits %s, want %s", in.NominalMs, in.Unit, got, w)
		}
		return nil

	case "backoff_advance":
		var in struct {
			NominalMs int64 `json:"nominal_ms"`
			MaxMs     int64 `json:"max_ms"`
		}
		var want struct {
			NextMs *int64 `json:"next_ms"`
		}
		if err := parseCase(c, &in, &want); err != nil {
			return err
		}
		if want.NextMs == nil {
			return errors.New("want.next_ms is missing")
		}
		got := d.advance(time.Duration(in.NominalMs)*time.Millisecond, time.Duration(in.MaxMs)*time.Millisecond)
		if w := time.Duration(*want.NextMs) * time.Millisecond; got != w {
			return fmt.Errorf("after %d ms (ceiling %d ms) comes %s, want %s", in.NominalMs, in.MaxMs, got, w)
		}
		return nil
	}
	return fmt.Errorf("unknown kind %q", c.Kind)
}

func TestTheDecisionVectors(t *testing.T) {
	cases := loadDecisions(t)
	counts := map[string]int{}
	for _, c := range cases {
		counts[c.Kind]++
		t.Run(c.Name, func(t *testing.T) {
			if err := reference.run(c); err != nil {
				t.Errorf("%v\n    why: %s", err, c.Why)
			}
		})
	}
	var summary []string
	for _, k := range decisionKinds {
		if counts[k] == 0 {
			t.Errorf("kind %q has no cases in %s", k, decisionsFile)
		}
		summary = append(summary, fmt.Sprintf("%s %d", k, counts[k]))
	}
	t.Logf("%d decision vectors: %s", len(cases), strings.Join(summary, ", "))
}

// Each mutant is the reference wrong at exactly ONE boundary, and must fail the
// named case — and so at least one of its kind. A mutant that passes is a rule
// the file does not pin.
func TestTheDecisionVectorsHaveTeeth(t *testing.T) {
	cases := loadDecisions(t)
	with := func(f func(*decisions)) decisions {
		d := reference
		f(&d)
		return d
	}
	for _, m := range []struct {
		name, kind, mustFail string
		d                    decisions
	}{
		{"readStamp keeps a future stamp", "stamp", "stamp_in_the_future_is_absent", with(func(d *decisions) {
			d.stamp = func(lastSent, _ time.Time) time.Time { return lastSent }
		})},
		{"gateOpen opens on an answer equal to the stamp", "gate", "gate_an_answer_equal_to_the_stamp_is_closed", with(func(d *decisions) {
			d.gate = func(answeredAt, stamp time.Time) bool {
				switch {
				case answeredAt.IsZero():
					return false
				case stamp.IsZero():
					return true
				}
				return !answeredAt.Before(stamp)
			}
		})},
		{"refreshThreshold drops the 60 s floor", "threshold", "threshold_a_short_lifetime_is_held_up_by_the_floor", with(func(d *decisions) {
			d.threshold = func(cr Credential) time.Duration {
				if cr.AccessIssuedAt.IsZero() {
					return refreshFloor
				}
				return cr.AccessExpiresAt.Sub(cr.AccessIssuedAt) / 3
			}
		})},
		{"refreshDue floors the third to whole milliseconds", "due", "due_a_third_that_is_not_a_whole_millisecond", with(func(d *decisions) {
			d.due = func(cr Credential, deviceNow time.Time) bool {
				if cr.AccessExpiresAt.IsZero() {
					return false
				}
				threshold := refreshFloor
				if !cr.AccessIssuedAt.IsZero() {
					threshold = max(refreshFloor, (cr.AccessExpiresAt.Sub(cr.AccessIssuedAt) / 3).Truncate(time.Millisecond))
				}
				return cr.AccessExpiresAt.Sub(deviceNow.Round(0).Add(cr.ClockOffset)) < threshold
			}
		})},
		{"refreshDue compares with <=", "due", "due_remaining_equal_to_the_threshold_is_not_due", with(func(d *decisions) {
			d.due = func(cr Credential, deviceNow time.Time) bool {
				if cr.AccessExpiresAt.IsZero() {
					return false
				}
				return cr.AccessExpiresAt.Sub(deviceNow.Round(0).Add(cr.ClockOffset)) <= refreshThreshold(cr)
			}
		})},
		{"refreshDelay has no cap", "delay", "delay_9_links_is_the_cap", with(func(d *decisions) {
			d.delay = func(links int) time.Duration {
				if links <= 0 {
					return 0
				}
				wait := refreshBackoffBase
				for range links - 1 {
					wait *= 2
				}
				return wait
			}
		})},
		{"refreshHoldAt checks the backoff before the gate", "hold", "hold_a_closed_gate_inside_the_delay_is_unreachable", with(func(d *decisions) {
			d.hold = func(links int, answeredAt, lastSent, now time.Time) RefreshHold {
				if links <= 0 {
					return RefreshNotHeld
				}
				stamp := readStamp(lastSent, now)
				if backoffPending(links, stamp, now) {
					return RefreshHeldBackoff
				}
				if !gateOpen(answeredAt, stamp) {
					return RefreshHeldUnreachable
				}
				return RefreshNotHeld
			}
		})},
		{"refusedHoldAt keeps a future raw stamp", "refused_hold", "refused_hold_a_future_raw_stamp_is_not_held", with(func(d *decisions) {
			d.refused = func(refused bool, links int, lastSent, now time.Time) bool {
				if !refused || links <= 0 {
					return false
				}
				return backoffPending(links, lastSent, now)
			}
		})},
		{"classifyClose consults retryable", "close", "close_1008_after_internal_not_retryable", with(func(d *decisions) {
			d.close = func(s websocket.StatusCode, p *wire.ServerError) closeVerdict {
				v, _ := classifyClose(s, p)
				if v == reconnectAtMaximum && !p.Retryable {
					return stopProtocolFailure
				}
				return v
			}
		})},
		// CANT-177's two halves, each failing the case it added: the session
		// loop's decoder made strict again, which refuses the frame, and a
		// classifyClose that stops on a code it does not know.
		{"preceding decoded by the server's strict decoder", "close", "close_1008_after_an_error_code_a_later_server_adds", with(func(d *decisions) {
			d.decode = wire.DecodeServerFrame
		})},
		{"classifyClose stops on a code it does not know", "close", "close_1008_after_an_error_code_a_later_server_adds", with(func(d *decisions) {
			d.close = func(s websocket.StatusCode, p *wire.ServerError) closeVerdict {
				if s == 1008 && p != nil && !p.Code.Valid() {
					return stopProtocolFailure
				}
				v, _ := classifyClose(s, p)
				return v
			}
		})},
		{"resetsRamp resets on any ready", "backoff_reset", "backoff_reset_ready_then_drop_does_not_reset", with(func(d *decisions) {
			d.reset = func(readied bool, _, _ time.Duration) bool { return readied }
		})},
		{"resetsRamp needs more than one whole interval", "backoff_reset", "backoff_reset_exactly_one_interval_resets", with(func(d *decisions) {
			d.reset = func(readied bool, readiedFor, interval time.Duration) bool { return readied && readiedFor > interval }
		})},
		{"unitFromBytes reads little endian", "backoff_draw", "backoff_draw_the_high_byte_is_first", with(func(d *decisions) {
			d.draw = func(b [4]byte) float64 { return float64(binary.LittleEndian.Uint32(b[:])) / math.MaxUint32 }
		})},
		{"unitFromBytes divides by 2^32", "backoff_draw", "backoff_draw_all_ones_is_the_maximum", with(func(d *decisions) {
			d.draw = func(b [4]byte) float64 { return float64(binary.BigEndian.Uint32(b[:])) / (1 << 32) }
		})},
		{"jitteredWait has no jitter", "backoff_jitter", "backoff_jitter_minimum_draw_is_0_8d", with(func(d *decisions) {
			d.jitter = func(nominal time.Duration, _ float64) time.Duration { return nominal }
		})},
		{"jitteredWait draws in rev 1's [d/2, d]", "backoff_jitter", "backoff_jitter_minimum_draw_is_0_8d", with(func(d *decisions) {
			d.jitter = func(nominal time.Duration, unit float64) time.Duration {
				return time.Duration(math.Round(float64(nominal) * (0.5 + 0.5*min(1, max(0, unit)))))
			}
		})},
		{"jitteredWait does not clamp the draw", "backoff_jitter", "backoff_jitter_a_draw_above_one_is_clamped", with(func(d *decisions) {
			d.jitter = func(nominal time.Duration, unit float64) time.Duration {
				return time.Duration(math.Round(float64(nominal) * (jitterFloor + (1-jitterFloor)*unit)))
			}
		})},
		{"advance has no ceiling", "backoff_advance", "backoff_advance_is_capped_at_the_ceiling", with(func(d *decisions) {
			d.advance = func(nominal, _ time.Duration) time.Duration { return nominal * 2 }
		})},
		{"Faults{NoChain}: a proposal minted and forgotten", "chain", "chain_a_newest_first_then_one_step_back_reusing_the_original_proposal",
			with(func(d *decisions) { d.faults = Faults{NoChain: true} })},
		{"Faults{ProposeAfresh}: a walk-back that mints a new proposal", "chain", "chain_a_newest_first_then_one_step_back_reusing_the_original_proposal",
			with(func(d *decisions) { d.faults = Faults{ProposeAfresh: true} })},
	} {
		t.Run(m.name, func(t *testing.T) {
			var of, failed int
			named := false
			for _, c := range cases {
				if c.Kind != m.kind {
					continue
				}
				of++
				if err := m.d.run(c); err != nil {
					failed++
					if c.Name == m.mustFail {
						named = true
						t.Logf("fails %s: %v", c.Name, err)
					}
				}
			}
			if failed == 0 {
				t.Fatalf("the mutant passes all %d %s cases: the vectors do not pin this rule", of, m.kind)
			}
			if !named {
				t.Errorf("the mutant fails %d of %d %s cases, but not %s", failed, of, m.kind, m.mustFail)
			}
			t.Logf("fails %d of %d %s cases", failed, of, m.kind)
		})
	}
}

// --- the chain transcripts ---------------------------------------------------

type vectorLink struct {
	Token    string `json:"token"`
	Proposal string `json:"proposal"`
}

type chainStep struct {
	Expect  vectorLink `json:"expect"`
	Respond struct {
		Status  int               `json:"status"`
		JSON    json.RawMessage   `json:"json"`
		Text    *string           `json:"text"`
		Headers map[string]string `json:"headers"`
		Drop    bool              `json:"drop"`
	} `json:"respond"`
}

// symbols binds each `$P<n>` to the proposal it names ON FIRST APPEARANCE, and
// holds every later appearance to it. Anything not starting with `$` is a
// literal fixture, compared as it is.
type symbols map[string]string

func (s symbols) match(what, want, got string) error {
	if !strings.HasPrefix(want, "$") {
		if got != want {
			return fmt.Errorf("%s %q, want %q", what, got, want)
		}
		return nil
	}
	if bound, ok := s[want]; ok && bound != got {
		return fmt.Errorf("%s %q, want %s = %q", what, got, want, bound)
	}
	s[want] = got
	return nil
}

// resolve is a symbol in a response or in `want`, which must already be bound:
// nothing the client has not yet presented can be answered or held.
func (s symbols) resolve(v string) (string, error) {
	if !strings.HasPrefix(v, "$") {
		return v, nil
	}
	bound, ok := s[v]
	if !ok {
		return "", fmt.Errorf("symbol %s is used before the client presented it", v)
	}
	return bound, nil
}

// resolveJSON replaces every symbol among a response body's string values.
func (s symbols) resolveJSON(raw json.RawMessage) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	var walk func(any) (any, error)
	walk = func(x any) (any, error) {
		switch x := x.(type) {
		case string:
			return s.resolve(x)
		case map[string]any:
			for k, e := range x {
				r, err := walk(e)
				if err != nil {
					return nil, err
				}
				x[k] = r
			}
		case []any:
			for i, e := range x {
				r, err := walk(e)
				if err != nil {
					return nil, err
				}
				x[i] = r
			}
		}
		return x, nil
	}
	v, err := walk(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// transcript is the scripted /refresh (ruling 1 → A). It holds no model of a
// server: each request is checked against the next step and answered with
// exactly the bytes the step carries. The first disagreement is kept, and every
// request after it gets a 500 — an unknown outcome, which ends the attempt.
type transcript struct {
	mu    sync.Mutex
	j     *Journal
	sym   symbols
	steps []chainStep
	next  int
	err   error
}

func (s *transcript) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func(format string, args ...any) {
		if s.err == nil {
			s.err = fmt.Errorf(format, args...)
		}
		http.Error(w, "decision vector mismatch", http.StatusInternalServerError)
	}
	if s.err != nil {
		fail("")
		return
	}
	if r.URL.Path != "/refresh" {
		fail("a request to %s; only /refresh is scripted", r.URL.Path)
		return
	}
	if s.next >= len(s.steps) {
		fail("request %d arrived, and this call scripts %d", s.next+1, len(s.steps))
		return
	}
	n, st := s.next+1, s.steps[s.next]
	s.next++

	// THE GENERATED DECODER, so a proposal that is not a wire Token fails here.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fail("step %d: read: %v", n, err)
		return
	}
	var req wire.RefreshRequest
	if err := json.Unmarshal(body, &req); err != nil {
		fail("step %d: the request does not decode as a wire RefreshRequest: %v", n, err)
		return
	}
	if req.ProposedRefreshToken == nil {
		fail("step %d: the request carries no proposal", n)
		return
	}
	got := ChainLink{Token: string(req.RefreshToken), Proposal: string(*req.ProposedRefreshToken)}
	if err := s.sym.match("presented token", st.Expect.Token, got.Token); err != nil {
		fail("step %d: %v", n, err)
		return
	}
	if err := s.sym.match("proposal", st.Expect.Proposal, got.Proposal); err != nil {
		fail("step %d: %v", n, err)
		return
	}
	// PERSISTED BEFORE SENT (TestTheChainIsPersistedBeforeTheRequestLeaves' check,
	// at every request): the link and its stamp are durable before it arrives.
	if !slices.Contains(s.j.Chain(), got) {
		fail("step %d: the journal does not hold the link being presented; it holds %v", n, s.j.Chain())
		return
	}
	if _, stamped := s.j.LastSent(); !stamped {
		fail("step %d: the journal holds no last_sent stamp for the request in flight", n)
		return
	}

	rs := st.Respond
	if rs.Drop {
		if rs.JSON != nil || rs.Text != nil || rs.Status != 0 || rs.Headers != nil {
			fail("step %d: a drop carries nothing else", n)
			return
		}
		drop(w)
		return
	}
	for k, v := range rs.Headers {
		if k != "Retry-After" || v != "0" {
			fail("step %d: the only header a vector may set is Retry-After: 0", n)
			return
		}
		w.Header().Set(k, v)
	}
	switch {
	case rs.JSON != nil && rs.Text == nil:
		out, err := s.sym.resolveJSON(rs.JSON)
		if err != nil {
			fail("step %d: respond: %v", n, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rs.Status)
		_, _ = w.Write(out)
	case rs.Text != nil && rs.JSON == nil:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(rs.Status)
		_, _ = io.WriteString(w, *rs.Text)
	default:
		fail("step %d: respond is exactly one of json, text or drop", n)
	}
}

func (d decisions) chain(c decisionCase) error {
	var in struct {
		Now   *string `json:"now"`
		Start struct {
			RefreshToken string       `json:"refresh_token"`
			Chain        []vectorLink `json:"chain"`
			LastSent     *string      `json:"last_sent"`
		} `json:"start"`
		Calls []struct {
			Steps []chainStep `json:"steps"`
		} `json:"calls"`
	}
	var want struct {
		RefreshToken *string      `json:"refresh_token"`
		Chain        []vectorLink `json:"chain"`
		Stamped      *bool        `json:"stamped"`
		Terminal     *bool        `json:"terminal"`
	}
	if err := parseCase(c, &in, &want); err != nil {
		return err
	}
	var now, lastSent time.Time
	if err := instants(&now, in.Now, &lastSent, in.Start.LastSent); err != nil {
		return err
	}
	if now.IsZero() || len(in.Calls) == 0 || want.RefreshToken == nil || want.Chain == nil || want.Stamped == nil || want.Terminal == nil {
		return errors.New("a chain case needs now, calls, and want.{refresh_token, chain, stamped, terminal}")
	}
	if n := len(in.Start.Chain); n > 0 && in.Start.Chain[0].Token != in.Start.RefreshToken {
		return errors.New("start.chain[0].token must be start.refresh_token (the Journal's invariant)")
	}

	// A PAIR EXPIRED AN HOUR BEFORE `now`, so the refresh is due whatever the
	// floor, on a clock that does not move.
	j := NewJournal()
	if err := j.Enroll(Credential{
		DeviceID: wire.Uuid(uuid.NewString()), AccessToken: padToken("access-before-rotation"), AccessExpiresAt: now.Add(-time.Hour),
		RefreshToken: in.Start.RefreshToken, RefreshExpiresAt: now.Add(24 * time.Hour),
	}); err != nil {
		return err
	}
	j.credMu.Lock()
	for _, l := range in.Start.Chain {
		j.chain = append(j.chain, ChainLink(l))
	}
	j.lastSent = lastSent
	j.credMu.Unlock()

	s := &transcript{j: j, sym: symbols{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	defer srv.Close()
	cl, err := New(Config{BaseURL: srv.URL, Journal: j, Refresh: true, Now: func() time.Time { return now }, Faults: d.faults})
	if err != nil {
		return err
	}
	defer cl.Close()

	for i, call := range in.Calls {
		s.mu.Lock()
		s.steps, s.next = call.Steps, 0
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = cl.RefreshIfDue(ctx) // what it returns is not a decision; the state it leaves is
		cancel()
		s.mu.Lock()
		err, arrived := s.err, s.next
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("call %d: %w", i+1, err)
		}
		if arrived != len(call.Steps) {
			return fmt.Errorf("call %d: %d requests arrived, and it scripts %d", i+1, arrived, len(call.Steps))
		}
	}

	// THE END STATE, READ FROM STATE: no counter is asserted.
	cr, _ := j.Credential()
	if w, err := s.sym.resolve(*want.RefreshToken); err != nil {
		return err
	} else if cr.RefreshToken != w {
		return fmt.Errorf("the journal holds refresh token %q, want %s = %q", cr.RefreshToken, *want.RefreshToken, w)
	}
	var wantChain []ChainLink
	for _, l := range want.Chain {
		tok, err := s.sym.resolve(l.Token)
		if err != nil {
			return err
		}
		prop, err := s.sym.resolve(l.Proposal)
		if err != nil {
			return err
		}
		wantChain = append(wantChain, ChainLink{Token: tok, Proposal: prop})
	}
	if got := j.Chain(); !slices.Equal(got, wantChain) {
		return fmt.Errorf("the chain is %v, want %v", got, wantChain)
	}
	if _, stamped := j.LastSent(); stamped != *want.Stamped {
		return fmt.Errorf("stamped %v, want %v", stamped, *want.Stamped)
	}
	if terminal := cl.Status().Terminal.Kind == TerminalCredential; terminal != *want.Terminal {
		return fmt.Errorf("credential terminal %v, want %v", terminal, *want.Terminal)
	}
	return nil
}

// --- reading the file ----------------------------------------------------------

// strict decodes, refusing a field the runner does not know: a misspelled key
// in a vector would otherwise read as absent and pass.
func strict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func parseCase(c decisionCase, in, want any) error {
	if err := strict(c.In, in); err != nil {
		return fmt.Errorf("in: %w", err)
	}
	if err := strict(c.Want, want); err != nil {
		return fmt.Errorf("want: %w", err)
	}
	return nil
}

// instants parses pairs of (destination, wire timestamp or nil). nil is absent:
// the zero time.
func instants(pairs ...any) error {
	for i := 0; i < len(pairs); i += 2 {
		dst, src := pairs[i].(*time.Time), pairs[i+1].(*string)
		if src == nil {
			*dst = time.Time{}
			continue
		}
		t, err := time.Parse(wireTimestampLayout, *src)
		if err != nil {
			return fmt.Errorf("instant %q: %w", *src, err)
		}
		*dst = t
	}
	return nil
}

func showInstant(t time.Time) string {
	if t.IsZero() {
		return "null"
	}
	return t.UTC().Format(wireTimestampLayout)
}

// decodePreceding is `preceding` through the GENERATED decoder the session loop
// uses, never built by hand: a close verdict over a frame the session loop
// could not produce would pin nothing.
func decodePreceding(decode func([]byte) (wire.ServerFrame, error), raw json.RawMessage) (*wire.ServerError, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	f, err := decode(raw)
	if err != nil {
		return nil, fmt.Errorf("preceding: the generated decoder refuses it: %w", err)
	}
	e, ok := f.(wire.ServerError)
	if !ok {
		return nil, fmt.Errorf("preceding: %T, want an error frame", f)
	}
	if e.ClientID != nil {
		return nil, errors.New("preceding: an error naming a client_id is that send's answer, never the session's (a malformed vector)")
	}
	return &e, nil
}

func same[T comparable](what string, got T, want *T) error {
	if want == nil {
		return fmt.Errorf("want.%s is missing", what)
	}
	if got != *want {
		return fmt.Errorf("%s %v, want %v", what, got, *want)
	}
	return nil
}
