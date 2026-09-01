package config

import (
	"log/slog"
	"testing"
	"time"
)

// setEnv clears every CATENARY_ variable the loader reads plus DATABASE_URL,
// then applies want. Tests that only set what they care about would otherwise
// inherit the developer's shell, and a config test that passes because of an
// exported variable is a config test that passes on one machine.
func setEnv(t *testing.T, want map[string]string) {
	t.Helper()
	for _, k := range []string{
		"CATENARY_DATABASE_URL", "DATABASE_URL", "CATENARY_PORT",
		"CATENARY_LOG_LEVEL", "CATENARY_LOG_FORMAT", "CATENARY_SHUTDOWN_GRACE",
	} {
		t.Setenv(k, "")
	}
	for k, v := range want {
		t.Setenv(k, v)
	}
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
