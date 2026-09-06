// Package config loads Catenary's configuration from the environment.
//
// Env-only, CATENARY_-prefixed, with a DATABASE_URL fallback so the
// shared-Postgres convention keeps working. No config files: a service with
// two places to look for a setting has two places for it to be wrong, and the
// one that is not in the compose file is the one nobody checks.
package config

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultPort is 4012, the next free slot in the estate's 40xx block. Read off
// construct-server's compose and .env rather than off what happened to be
// listening: 4000 vox-loop, 4001 cook-book, 4002 switchyard, 4003 centrifuge,
// 4004 centrifuge-web, 4005 lyceum, 4006 purser, 4007 interlock, 4008 amber,
// 4009 placard and chronicle, 4010 the Signet host daemon, 4011 the shared ASR
// service. Amber took 4008 as "next free after interlock 4007"; this is the
// same rule one turn later.
//
// Catenary will most likely publish NO port at all — Chronicle and the ASR
// service both sit on construct_net behind Traefik with no `ports:` mapping,
// and CANT-16 puts Catenary on the same split entrypoint. The number then only
// has to be unique to be legible. Overridable with CATENARY_PORT, which is what
// a reallocation should use rather than a rebuild.
const DefaultPort = 4012

// DefaultShutdownGrace is how long in-flight work has to finish on SIGTERM.
//
// Twenty seconds, and the reason is specific to this service rather than
// copied: Catenary's long-lived connection is a WebSocket, and R1 measured a
// client that believes it is connected for 70-105 s after it is not. A grace
// window shorter than a clean close turns every deploy into a reconnect storm
// against a server that is still starting.
const DefaultShutdownGrace = 20 * time.Second

// Config is the process-wide configuration. Every field is set from exactly one
// environment variable, named in its comment, so `grep CATENARY_` over this
// file is the complete list.
type Config struct {
	// DatabaseURL is CATENARY_DATABASE_URL, falling back to DATABASE_URL.
	// Required: there is no useful thing this process does without it, and a
	// service that boots without its database only fails later, further away.
	DatabaseURL string

	// Addr is the listen address, built from CATENARY_PORT.
	Addr string

	// LogLevel is CATENARY_LOG_LEVEL: debug | info | warn | error.
	LogLevel slog.Level

	// LogFormat is CATENARY_LOG_FORMAT: json | text.
	LogFormat string

	// ShutdownGrace is CATENARY_SHUTDOWN_GRACE.
	ShutdownGrace time.Duration

	// MaxMessageBytes is CATENARY_MAX_MESSAGE_BYTES: the UTF-8 byte bound on a
	// message body. MaxAttachments is CATENARY_MAX_ATTACHMENTS.
	//
	// ZERO MEANS UNSET, and the defaults are deliberately NOT restated here.
	// They live in store.DefaultLimits, which is what enforces them, and the
	// composition root merges these over it. Two copies of a bound that must
	// agree is one copy too many, and this file cannot import the store
	// without inverting the layering.
	MaxMessageBytes int
	MaxAttachments  int
}

// Load reads the environment and validates it, or returns the first error.
//
// Every failure names the variable. A config error that says "invalid port"
// without saying which variable held it is a config error someone debugs by
// bisecting their compose file.
func Load() (Config, error) {
	var c Config
	var err error

	c.DatabaseURL = firstNonEmpty(os.Getenv("CATENARY_DATABASE_URL"), os.Getenv("DATABASE_URL"))
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("config: CATENARY_DATABASE_URL (or DATABASE_URL) is required")
	}

	port := DefaultPort
	if v := strings.TrimSpace(os.Getenv("CATENARY_PORT")); v != "" {
		port, err = strconv.Atoi(v)
		if err != nil || port < 1 || port > 65535 {
			return c, fmt.Errorf("config: CATENARY_PORT %q is not a valid port", v)
		}
	}
	c.Addr = fmt.Sprintf(":%d", port)

	c.LogLevel, err = parseLevel(firstNonEmpty(os.Getenv("CATENARY_LOG_LEVEL"), "info"))
	if err != nil {
		return c, err
	}

	// JSON by default: Datadog parses it into attributes with no pipeline
	// config, and Dozzle renders it fine. text is for a human at a terminal.
	c.LogFormat = strings.ToLower(firstNonEmpty(os.Getenv("CATENARY_LOG_FORMAT"), "json"))
	if c.LogFormat != "json" && c.LogFormat != "text" {
		return c, fmt.Errorf("config: CATENARY_LOG_FORMAT %q is not json or text", c.LogFormat)
	}

	c.ShutdownGrace = DefaultShutdownGrace
	if v := strings.TrimSpace(os.Getenv("CATENARY_SHUTDOWN_GRACE")); v != "" {
		c.ShutdownGrace, err = time.ParseDuration(v)
		if err != nil || c.ShutdownGrace <= 0 {
			return c, fmt.Errorf("config: CATENARY_SHUTDOWN_GRACE %q is not a positive duration", v)
		}
	}

	// The name is passed AND the value is read here, rather than the helper
	// doing its own lookup from the name. config_test.go derives the list of
	// variables to clear between tests by scanning this file for os.Getenv
	// calls with a LITERAL argument, so a name that reaches the environment
	// through a parameter is a name that list silently stops clearing — the
	// failure that helper's own comment warns about.
	//
	// Writing the pattern out in a comment does not work either: the scan is a
	// regex over this file and does not know a comment from code, so a spelled
	// example would invent a variable that does not exist. It did, once.
	if c.MaxMessageBytes, err = positiveInt("CATENARY_MAX_MESSAGE_BYTES", os.Getenv("CATENARY_MAX_MESSAGE_BYTES")); err != nil {
		return c, err
	}
	if c.MaxAttachments, err = positiveInt("CATENARY_MAX_ATTACHMENTS", os.Getenv("CATENARY_MAX_ATTACHMENTS")); err != nil {
		return c, err
	}

	return c, nil
}

// positiveInt reads an optional positive bound. Unset returns 0, which the
// composition root reads as "keep the store's default".
//
// Zero is REFUSED rather than accepted as a bound. An operator setting a limit
// to 0 means "off" far more often than "refuse everything", and a store that
// refuses every send because a variable was misread is a worse outage than a
// startup failure that names the variable.
func positiveInt(name, raw string) (int, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("config: %s %q is not a positive integer", name, v)
	}
	return n, nil
}

// Logger builds the process logger. Structured to w, always — there is no
// unstructured mode, because the shape is what Dozzle and Datadog read.
//
// io.Writer rather than *os.File: the only caller passes os.Stdout, but the
// narrower type meant the text branch could not be asserted against a buffer
// and so went untested, while api's tests built their own slog.Logger rather
// than going through here.
func (c Config) Logger(w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.LogLevel}
	if c.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("config: CATENARY_LOG_LEVEL %q is not debug/info/warn/error", s)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
