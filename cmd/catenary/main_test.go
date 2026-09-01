package main

import (
	"os"
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
	cfg := config.Config{DatabaseURL: "postgres://x/y", Addr: ":4012", LogFormat: "json"}
	d := setup(cfg, cfg.Logger(os.Stdout), nil)
	if d.logger == nil || d.router == nil {
		t.Fatal("setup returned an incomplete deps")
	}
	if d.store != nil {
		t.Error("setup invented a store it was not given")
	}
}

// Every CATENARY_ variable the loader reads is named in the usage text. A
// config surface that is env-only is only documented if `catenary --help` is
// the documentation, and the two drift the moment a variable is added.
func TestUsageNamesEveryEnvVar(t *testing.T) {
	got := usageText()
	for _, v := range []string{
		"CATENARY_DATABASE_URL", "DATABASE_URL", "CATENARY_PORT",
		"CATENARY_LOG_LEVEL", "CATENARY_LOG_FORMAT", "CATENARY_SHUTDOWN_GRACE",
	} {
		if !strings.Contains(got, v) {
			t.Errorf("usage does not name %s", v)
		}
	}
}
