package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
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
