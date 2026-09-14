package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/store"
)

func TestRunRejectsUnknownSubcommands(t *testing.T) {
	for _, args := range [][]string{{}, {"srve"}, {"migrat"}} {
		if err := run(args); err == nil {
			t.Errorf("run(%q) succeeded, want an error", args)
		}
	}
}

func TestRunHelp(t *testing.T) {
	for _, a := range []string{"-h", "--help", "help", "version"} {
		if err := run([]string{a}); err != nil {
			t.Errorf("run(%q) = %v, want nil", a, err)
		}
	}
}

func TestBuildVersionFallsBackToDev(t *testing.T) {
	if got := buildVersion(); got != "dev" {
		t.Errorf("buildVersion() on an unstamped build = %q, want dev", got)
	}
}

// The composition root builds a usable router from nothing but a Config.
func TestSetup(t *testing.T) {
	cfg := config.Config{DatabaseURL: "postgres://x/y", Addr: ":4012", LogFormat: "json"}
	d := setup(cfg, cfg.Logger(os.Stdout), nil)
	if d.logger == nil || d.router == nil {
		t.Fatal("setup returned an incomplete deps")
	}
	if d.store != nil {
		t.Error("setup invented a store it was not given")
	}
}

// CANT-85. An attachment bound above the store's ceiling is a STARTUP error that
// names the variable the operator set — not a panic out of store.New after the
// database wait, and not a Limits field name the operator never typed.
func TestLimitsFromNamesTheVariableThatIsOutOfRange(t *testing.T) {
	_, err := limitsFrom(config.Config{MaxAttachments: store.MaxAttachmentsCeiling + 1})
	if err == nil {
		t.Fatalf("CATENARY_MAX_ATTACHMENTS=%d was accepted; the ceiling is %d",
			store.MaxAttachmentsCeiling+1, store.MaxAttachmentsCeiling)
	}
	if !strings.Contains(err.Error(), "CATENARY_MAX_ATTACHMENTS") {
		t.Errorf("the error does not name the variable: %v", err)
	}

	// At the ceiling is accepted, and unset keeps the store's defaults.
	got, err := limitsFrom(config.Config{MaxAttachments: store.MaxAttachmentsCeiling})
	if err != nil || got.MaxAttachments != store.MaxAttachmentsCeiling {
		t.Errorf("limitsFrom at the ceiling = %+v, %v", got, err)
	}
	got, err = limitsFrom(config.Config{})
	if err != nil || got != store.DefaultLimits() {
		t.Errorf("limitsFrom with nothing set = %+v, %v; want DefaultLimits", got, err)
	}
}

// CANT-23. A dial outside the wire schema's own bounds on ServerReady
// (interval [5, 90], limit >= 1) is a STARTUP error naming the variable — the
// same shape TestLimitsFromNamesTheVariableThatIsOutOfRange asserts for
// CATENARY_MAX_ATTACHMENTS, so `ready` can never announce a number its own
// generated decoders would refuse.
func TestHeartbeatBoundsFromRejectsOutOfRange(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"interval below the schema's floor", config.Config{HeartbeatIntervalSec: 4}, "CATENARY_HEARTBEAT_INTERVAL_SEC"},
		{"interval above the schema's ceiling", config.Config{HeartbeatIntervalSec: 91}, "CATENARY_HEARTBEAT_INTERVAL_SEC"},
		{"limit below the schema's floor", config.Config{MissedPongLimit: -1}, "CATENARY_MISSED_PONG_LIMIT"},
		// The schema leaves missed_pong_limit unbounded above; heartbeatWindow
		// does not survive that (a large enough limit overflows int64
		// nanoseconds into a negative duration, which severs every session at
		// `ready`), so this function draws its own ceiling.
		{"limit above the overflow ceiling", config.Config{MissedPongLimit: missedPongLimitCeiling + 1}, "CATENARY_MISSED_PONG_LIMIT"},
		{"limit far above the overflow ceiling", config.Config{MissedPongLimit: 200_000_000}, "CATENARY_MISSED_PONG_LIMIT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := heartbeatBoundsFrom(tc.cfg)
			if err == nil {
				t.Fatalf("heartbeatBoundsFrom(%+v) succeeded, want an error", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name the variable: %v", err)
			}
		})
	}

	// At the bounds, and unset, are both accepted.
	for _, cfg := range []config.Config{
		{HeartbeatIntervalSec: 5, MissedPongLimit: 1},
		{HeartbeatIntervalSec: 90, MissedPongLimit: 1},
		{HeartbeatIntervalSec: 90, MissedPongLimit: missedPongLimitCeiling},
		{},
	} {
		if err := heartbeatBoundsFrom(cfg); err != nil {
			t.Errorf("heartbeatBoundsFrom(%+v) = %v, want nil", cfg, err)
		}
	}
}

// setup threads an operator's dial straight into api.Deps, which merges zero
// with api.DefaultHeartbeatIntervalSec / api.DefaultMissedPongLimit itself —
// so setup needs no merge logic of its own, only the pass-through. Proven end
// to end, over a real socket through the REAL router setup builds, by
// cmd/catenary/socket_test.go's TestTheHeartbeatDialFlowsThroughSetup; this
// pins that setup does not build without the two new fields wired at all.
func TestSetupBuildsWithAHeartbeatDial(t *testing.T) {
	cfg := config.Config{DatabaseURL: "postgres://x/y", Addr: ":4012", LogFormat: "json",
		HeartbeatIntervalSec: 45, MissedPongLimit: 3}
	d := setup(cfg, cfg.Logger(os.Stdout), nil)
	if d.router == nil {
		t.Fatal("setup returned an incomplete deps")
	}
}

// Every variable the loader reads is named in the usage text. A config surface
// that is env-only is only documented if `catenary --help` IS the
// documentation, and the two drift the moment a variable is added.
//
// The list is DERIVED from config.go rather than written here. A hand-kept
// slice cannot catch the drift this test is named for: adding an
// os.Getenv("CATENARY_MAX_UPLOAD_MB") to Load and stopping there leaves the
// slice unchanged, the test green, and --help silently short one variable —
// which is exactly the scenario, and it was the shape of the first version of
// this test.
func TestUsageNamesEveryEnvVar(t *testing.T) {
	got := usageText()
	read := envVarsReadBy(t, "../../internal/config/config.go")
	if len(read) < 5 {
		t.Fatalf("found only %d os.Getenv calls in config.go — the scan is broken, not the config", len(read))
	}
	for _, v := range read {
		if !strings.Contains(got, v) {
			t.Errorf("config.Load reads %s and `catenary --help` does not name it", v)
		}
	}
}

// envVarsReadBy returns every environment variable named in an os.Getenv call
// in the given Go source file, sorted and deduplicated.
func envVarsReadBy(t *testing.T, path string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`os\.Getenv\("([A-Z_]+)"\)`).FindAllStringSubmatch(string(src), -1) {
		seen[m[1]] = true
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
