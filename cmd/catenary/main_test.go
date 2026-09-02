package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/magos/catenary/internal/config"
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
	d := setup(config.Config{
		DatabaseURL: "postgres://x/y",
		Addr:        ":4012",
		LogFormat:   "json",
	})
	if d.logger == nil || d.router == nil {
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
