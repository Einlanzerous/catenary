package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// setEnv clears every variable Load reads, then applies want. Tests that only
// set what they care about would otherwise inherit the developer's shell, and a
// config test that passes because of an exported variable is a config test that
// passes on one machine.
//
// The list is DERIVED from this package's own source rather than written out.
// A hand-kept copy silently stops clearing the next variable somebody adds,
// which is the same failure the usage-text guard in cmd/catenary had.
func setEnv(t *testing.T, want map[string]string) {
	t.Helper()
	for _, k := range envVarsReadByLoad(t) {
		t.Setenv(k, "")
	}
	for k, v := range want {
		t.Setenv(k, v)
	}
}

func envVarsReadByLoad(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`os\.Getenv\("([A-Z_]+)"\)`).FindAllStringSubmatch(string(src), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	if len(out) < 5 {
		t.Fatalf("found only %d os.Getenv calls in config.go — the scan is broken, not the config", len(out))
	}
	return out
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, map[string]string{"CATENARY_DATABASE_URL": "postgres://x/y"})

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DatabaseURL != "postgres://x/y" {
		t.Errorf("DatabaseURL = %q", c.DatabaseURL)
	}
	if want := ":4012"; c.Addr != want {
		t.Errorf("Addr = %q, want %q", c.Addr, want)
	}
	if c.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", c.LogLevel)
	}
	if c.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json — Datadog parses it with no pipeline config", c.LogFormat)
	}
	if c.ShutdownGrace != 20*time.Second {
		t.Errorf("ShutdownGrace = %v, want 20s", c.ShutdownGrace)
	}
}

// The fallback is the shared-Postgres convention: DATABASE_URL is what the
// compose file sets estate-wide, and CATENARY_DATABASE_URL overrides it.
func TestDatabaseURLFallback(t *testing.T) {
	setEnv(t, map[string]string{"DATABASE_URL": "postgres://fallback/db"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DatabaseURL != "postgres://fallback/db" {
		t.Errorf("DatabaseURL = %q, want the DATABASE_URL fallback", c.DatabaseURL)
	}

	setEnv(t, map[string]string{
		"DATABASE_URL":          "postgres://fallback/db",
		"CATENARY_DATABASE_URL": "postgres://prefixed/db",
	})
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DatabaseURL != "postgres://prefixed/db" {
		t.Errorf("DatabaseURL = %q, want the CATENARY_-prefixed value to win", c.DatabaseURL)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"no database url", map[string]string{}},
		{"port not a number", map[string]string{"CATENARY_DATABASE_URL": "x", "CATENARY_PORT": "http"}},
		{"port out of range", map[string]string{"CATENARY_DATABASE_URL": "x", "CATENARY_PORT": "70000"}},
		{"port zero", map[string]string{"CATENARY_DATABASE_URL": "x", "CATENARY_PORT": "0"}},
		{"unknown log level", map[string]string{"CATENARY_DATABASE_URL": "x", "CATENARY_LOG_LEVEL": "verbose"}},
		{"unknown log format", map[string]string{"CATENARY_DATABASE_URL": "x", "CATENARY_LOG_FORMAT": "logfmt"}},
		{"grace not a duration", map[string]string{"CATENARY_DATABASE_URL": "x", "CATENARY_SHUTDOWN_GRACE": "20"}},
		{"grace zero", map[string]string{"CATENARY_DATABASE_URL": "x", "CATENARY_SHUTDOWN_GRACE": "0s"}},
		{"grace negative", map[string]string{"CATENARY_DATABASE_URL": "x", "CATENARY_SHUTDOWN_GRACE": "-5s"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			if _, err := Load(); err == nil {
				t.Fatal("Load succeeded, want an error naming the variable")
			}
		})
	}
}

func TestLogLevels(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "DEBUG": slog.LevelDebug,
		"info": slog.LevelInfo, "warn": slog.LevelWarn,
		"warning": slog.LevelWarn, "error": slog.LevelError,
	} {
		setEnv(t, map[string]string{"CATENARY_DATABASE_URL": "x", "CATENARY_LOG_LEVEL": in})
		c, err := Load()
		if err != nil {
			t.Fatalf("Load(%q): %v", in, err)
		}
		if c.LogLevel != want {
			t.Errorf("LogLevel(%q) = %v, want %v", in, c.LogLevel, want)
		}
	}
}

// CATENARY_LOG_FORMAT is a documented, validated, user-reachable setting, and
// until Logger took an io.Writer there was no way to assert either branch
// without writing a real file — so the text branch had no test at all.
func TestLoggerHonoursTheFormat(t *testing.T) {
	for _, tc := range []struct {
		format string
		json   bool
	}{{"json", true}, {"text", false}} {
		t.Run(tc.format, func(t *testing.T) {
			setEnv(t, map[string]string{"CATENARY_DATABASE_URL": "x", "CATENARY_LOG_FORMAT": tc.format})
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			var buf bytes.Buffer
			c.Logger(&buf).Info("hello", "k", "v")

			line := strings.TrimSpace(buf.String())
			if line == "" {
				t.Fatal("logger wrote nothing")
			}
			isJSON := json.Unmarshal([]byte(line), &map[string]any{}) == nil
			if isJSON != tc.json {
				t.Errorf("format %q produced JSON=%v, want %v: %q", tc.format, isJSON, tc.json, line)
			}
			if !strings.Contains(line, "hello") || !strings.Contains(line, "k") {
				t.Errorf("log line lost its content: %q", line)
			}
		})
	}
}

// CANT-83. Both bounds are OPTIONAL and zero means unset: the defaults live in
// store.DefaultLimits, which is what enforces them, and the composition root
// merges these over it. Restating the numbers here would be a second copy that
// has to agree with the first.
func TestSendBoundsAreOptionalAndZeroMeansUnset(t *testing.T) {
	setEnv(t, map[string]string{"CATENARY_DATABASE_URL": "postgres://x/y"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxMessageBytes != 0 || c.MaxAttachments != 0 {
		t.Errorf("unset bounds = %d/%d, want 0/0 so the store's defaults stand",
			c.MaxMessageBytes, c.MaxAttachments)
	}

	setEnv(t, map[string]string{
		"CATENARY_DATABASE_URL":      "postgres://x/y",
		"CATENARY_MAX_MESSAGE_BYTES": "2048",
		"CATENARY_MAX_ATTACHMENTS":   "4",
	})
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxMessageBytes != 2048 || c.MaxAttachments != 4 {
		t.Errorf("bounds = %d/%d, want 2048/4", c.MaxMessageBytes, c.MaxAttachments)
	}
}

// Zero is REFUSED rather than taken as a bound. An operator setting a limit to
// 0 means "off" far more often than "refuse everything", and a store that
// refuses every send because a variable was misread is a worse outage than a
// startup failure naming the variable.
func TestSendBoundsRejectNonPositiveValues(t *testing.T) {
	for _, tc := range []struct{ name, key, val string }{
		{"bytes zero", "CATENARY_MAX_MESSAGE_BYTES", "0"},
		{"bytes negative", "CATENARY_MAX_MESSAGE_BYTES", "-1"},
		{"bytes not a number", "CATENARY_MAX_MESSAGE_BYTES", "16k"},
		{"attachments zero", "CATENARY_MAX_ATTACHMENTS", "0"},
		{"attachments negative", "CATENARY_MAX_ATTACHMENTS", "-4"},
		{"attachments not a number", "CATENARY_MAX_ATTACHMENTS", "many"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, map[string]string{"CATENARY_DATABASE_URL": "postgres://x/y", tc.key: tc.val})
			_, err := Load()
			if err == nil {
				t.Fatal("Load succeeded, want an error naming the variable")
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error does not name the variable: %v", err)
			}
		})
	}
}

// The derived clear-list must actually see the two new variables, or every test
// above inherits whatever the developer's shell has set.
func TestTheNewBoundsAreVisibleToTheEnvScanner(t *testing.T) {
	got := envVarsReadByLoad(t)
	for _, want := range []string{"CATENARY_MAX_MESSAGE_BYTES", "CATENARY_MAX_ATTACHMENTS"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not visible to envVarsReadByLoad — it reaches os.Getenv through a "+
				"parameter, so setEnv will stop clearing it: %v", want, got)
		}
	}
}

// CANT-23. Both are OPTIONAL and zero means unset, the same convention as the
// send bounds: the defaults — R1's 35 s / 2 — live in internal/api and the
// composition root merges these over them.
func TestHeartbeatDialIsOptionalAndZeroMeansUnset(t *testing.T) {
	setEnv(t, map[string]string{"CATENARY_DATABASE_URL": "postgres://x/y"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HeartbeatIntervalSec != 0 || c.MissedPongLimit != 0 {
		t.Errorf("unset heartbeat dial = %d/%d, want 0/0 so api's defaults stand",
			c.HeartbeatIntervalSec, c.MissedPongLimit)
	}

	setEnv(t, map[string]string{
		"CATENARY_DATABASE_URL":           "postgres://x/y",
		"CATENARY_HEARTBEAT_INTERVAL_SEC": "45",
		"CATENARY_MISSED_PONG_LIMIT":      "3",
	})
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HeartbeatIntervalSec != 45 || c.MissedPongLimit != 3 {
		t.Errorf("heartbeat dial = %d/%d, want 45/3", c.HeartbeatIntervalSec, c.MissedPongLimit)
	}
}

// Load only floors these at a positive integer (positiveInt, shared with the
// send bounds). The wire schema's own bounds — interval [5, 90], and the
// floor of 1 this already matches for the limit — are checked at the
// composition root; see cmd/catenary's TestHeartbeatBoundsFromRejectsOutOfRange.
func TestHeartbeatDialRejectsNonPositiveValues(t *testing.T) {
	for _, tc := range []struct{ name, key, val string }{
		{"interval zero", "CATENARY_HEARTBEAT_INTERVAL_SEC", "0"},
		{"interval negative", "CATENARY_HEARTBEAT_INTERVAL_SEC", "-1"},
		{"interval not a number", "CATENARY_HEARTBEAT_INTERVAL_SEC", "soon"},
		{"limit zero", "CATENARY_MISSED_PONG_LIMIT", "0"},
		{"limit negative", "CATENARY_MISSED_PONG_LIMIT", "-2"},
		{"limit not a number", "CATENARY_MISSED_PONG_LIMIT", "several"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, map[string]string{"CATENARY_DATABASE_URL": "postgres://x/y", tc.key: tc.val})
			_, err := Load()
			if err == nil {
				t.Fatal("Load succeeded, want an error naming the variable")
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error does not name the variable: %v", err)
			}
		})
	}
}

// The same drift guard as TestTheNewBoundsAreVisibleToTheEnvScanner, for the
// heartbeat dial.
func TestTheHeartbeatDialIsVisibleToTheEnvScanner(t *testing.T) {
	got := envVarsReadByLoad(t)
	for _, want := range []string{"CATENARY_HEARTBEAT_INTERVAL_SEC", "CATENARY_MISSED_PONG_LIMIT"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not visible to envVarsReadByLoad — it reaches os.Getenv through a "+
				"parameter, so setEnv will stop clearing it: %v", want, got)
		}
	}
}

// --- CANT-131: the provisioning surface's two variables ----------------------

// provisionToken is exactly MinProvisionTokenBytes long and a LITERAL, so that
// the assertion which searches every refusal for its prefixes is the same every
// run.
//
// IT READS AS A SENTENCE RATHER THAN AS A CREDENTIAL, ON ci.yml's OWN ARGUMENT.
// The first version of this test used 32 random base64url characters, which is
// what Signet actually generates — and GitGuardian reported it as a Generic High
// Entropy Secret on the pull request, exactly as ci.yml predicted for the
// password-shaped Postgres literal it deleted for the same reason: "a check that
// is routinely dismissed is a check nobody reads. Removing the literal removes
// the finding at its source instead of teaching people to wave it through." What
// these tests need from this value is its LENGTH, its mixed case and its
// distinctive prefix — none of which requires entropy.
//
// The capital T in `Test` is load bearing in internal/provision's own table: a
// credential that differs from this one only in case must be refused, and an
// all-lowercase constant could not express that.
const provisionToken = "not-a-secret-just-a-Test-token-1"

// Criterion 9's boot half — WITH NEITHER VARIABLE THE SURFACE DOES NOT EXIST, and
// that is the ordinary case: every deployment of this service before CANT-131, and
// every test in every other package, runs with both unset and must be completely
// unaffected.
func TestTheProvisioningSurfaceIsAbsentUnlessItIsConfigured(t *testing.T) {
	setEnv(t, map[string]string{"CATENARY_DATABASE_URL": "postgres://x/y"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load with neither provisioning variable set: %v", err)
	}
	if c.ProvisionAddr != "" || c.ProvisionToken != "" {
		t.Errorf("unset provisioning config = %q / %d bytes, want both empty", c.ProvisionAddr, len(c.ProvisionToken))
	}
	if c.ProvisioningEnabled() {
		t.Error("ProvisioningEnabled() is true with neither variable set — the composition root would " +
			"build a credential-minting surface for a deployment that was never given a credential")
	}
}

// BOTH OR NEITHER, AND A FLOOR ON THE CREDENTIAL. Each refusal names the variable,
// because a boot failure that says "invalid provisioning configuration" is a boot
// failure somebody debugs by bisecting a compose file.
func TestLoadRefusesAHalfConfiguredProvisioningSurface(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		names string
	}{
		{
			"an address with no token",
			map[string]string{"CATENARY_PROVISION_ADDR": ":4013"},
			"CATENARY_PROVISION_TOKEN",
		},
		{
			"a token with no address",
			map[string]string{"CATENARY_PROVISION_TOKEN": provisionToken},
			"CATENARY_PROVISION_ADDR",
		},
		{
			// ONE BYTE UNDER THE FLOOR. The interesting boundary is not "short",
			// it is "one short" — a check written with >= where it meant > passes
			// every test whose bad value is obviously bad.
			"a token one byte under the floor",
			map[string]string{"CATENARY_PROVISION_ADDR": ":4013", "CATENARY_PROVISION_TOKEN": provisionToken[:MinProvisionTokenBytes-1]},
			"CATENARY_PROVISION_TOKEN",
		},
		{
			"a token that is only whitespace",
			map[string]string{"CATENARY_PROVISION_ADDR": ":4013", "CATENARY_PROVISION_TOKEN": "   "},
			"CATENARY_PROVISION_TOKEN",
		},
		{
			// The obvious mistake: the routed port copied into the second address.
			"the address the routed listener already holds",
			map[string]string{"CATENARY_PROVISION_ADDR": ":4012", "CATENARY_PROVISION_TOKEN": provisionToken},
			"CATENARY_PROVISION_ADDR",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"CATENARY_DATABASE_URL": "postgres://x/y"}
			for k, v := range tc.env {
				env[k] = v
			}
			setEnv(t, env)

			_, err := Load()
			if err == nil {
				t.Fatal("Load succeeded; a half-configured provisioning surface must refuse to boot")
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal does not name %s: %v", tc.names, err)
			}

			// AND IT NEVER ECHOES THE CREDENTIAL. Every prefix from four bytes up,
			// and the whole thing.
			//
			// THE LENGTH IS ALLOWED HERE AND IS NOT ALLOWED IN THE 401's LOG LINE,
			// and the difference is who is reading. This message goes to the
			// operator who just set the variable and needs to know their value is
			// 31 bytes; that log line goes wherever logs go, about a credential
			// somebody else presented, and a length there narrows a guess.
			for n := 4; n <= len(provisionToken); n++ {
				if strings.Contains(err.Error(), provisionToken[:n]) {
					t.Fatalf("the refusal echoes the first %d bytes of the credential: %v", n, err)
				}
			}
		})
	}
}

func TestLoadAcceptsAFullyConfiguredProvisioningSurface(t *testing.T) {
	setEnv(t, map[string]string{
		"CATENARY_DATABASE_URL":    "postgres://x/y",
		"CATENARY_PROVISION_ADDR":  " :4013 ",
		"CATENARY_PROVISION_TOKEN": provisionToken,
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The ADDRESS is trimmed, because a stray space in a compose file is a typo.
	if c.ProvisionAddr != ":4013" {
		t.Errorf("ProvisionAddr = %q, want the trimmed :4013", c.ProvisionAddr)
	}
	// The TOKEN is not, because Purser presents it verbatim and silently trimming
	// it here would make the two services disagree about the secret whenever one
	// of them was pasted with trailing whitespace — a 401 with no visible cause.
	if c.ProvisionToken != provisionToken {
		t.Errorf("ProvisionToken was altered by Load (%d bytes, want %d)", len(c.ProvisionToken), len(provisionToken))
	}
	if !c.ProvisioningEnabled() {
		t.Error("ProvisioningEnabled() is false with both variables set")
	}

	// AT the floor is accepted; the boundary is tested from both sides.
	if len(provisionToken) != MinProvisionTokenBytes {
		t.Fatalf("this test's token is %d bytes; it is meant to sit exactly at the floor of %d",
			len(provisionToken), MinProvisionTokenBytes)
	}
}

// A token carrying trailing whitespace is still measured on its raw bytes, so a
// 32-byte value that is 30 bytes and two spaces passes the floor. Stated as a test
// rather than left implicit, because it is the cost of not trimming and somebody
// will ask.
func TestTheTokenIsMeasuredOnTheBytesPurserWillPresent(t *testing.T) {
	padded := provisionToken[:MinProvisionTokenBytes-2] + "  "
	setEnv(t, map[string]string{
		"CATENARY_DATABASE_URL":    "postgres://x/y",
		"CATENARY_PROVISION_ADDR":  ":4013",
		"CATENARY_PROVISION_TOKEN": padded,
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ProvisionToken != padded {
		t.Errorf("ProvisionToken = %d bytes, want the %d it was given, whitespace included",
			len(c.ProvisionToken), len(padded))
	}
}

// The same drift guard as the two above, for the provisioning pair: a variable the
// scanner cannot see is a variable setEnv stops clearing, and then every test in
// this file inherits whatever the developer's shell has set — including, here, a
// real credential.
func TestTheProvisioningVariablesAreVisibleToTheEnvScanner(t *testing.T) {
	got := envVarsReadByLoad(t)
	for _, want := range []string{"CATENARY_PROVISION_ADDR", "CATENARY_PROVISION_TOKEN"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s is not visible to envVarsReadByLoad — it reaches os.Getenv through a "+
				"parameter, so setEnv will stop clearing it: %v", want, got)
		}
	}
}
