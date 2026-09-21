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

	// HeartbeatIntervalSec is CATENARY_HEARTBEAT_INTERVAL_SEC and
	// MissedPongLimit is CATENARY_MISSED_PONG_LIMIT: CANT-23's deployed dial,
	// announced verbatim in `ready` and what the socket's own severance
	// watchdog derives its window from.
	//
	// ZERO MEANS UNSET, same convention as the two bounds above and for the
	// same reason: the defaults — R1's 35 s / 2 — live in internal/api as
	// DefaultHeartbeatIntervalSec / DefaultMissedPongLimit, and the
	// composition root merges these over them. What Load checks here is only
	// that a SET value is a positive integer; the wire schema's own bounds
	// (interval [5, 90], limit >= 1) are checked at the composition root,
	// before anything slow starts, the same way MaxAttachmentsCeiling is —
	// so `ready` can never announce a value its own generated decoders
	// would refuse.
	HeartbeatIntervalSec int
	MissedPongLimit      int

	// ProvisionAddr is CATENARY_PROVISION_ADDR and ProvisionToken is
	// CATENARY_PROVISION_TOKEN: CANT-131's provisioning surface — the listen
	// address of the SECOND http.Server, and the one static service credential
	// that opens it.
	//
	// BOTH EMPTY MEANS THE SURFACE DOES NOT EXIST. Not "exists and refuses
	// everything": no listener is opened and no handler is registered, so a
	// deployment that has not been given a credential has no door to attack.
	// That is ruling 2's fail-closed clause, and it is the reason these are two
	// variables with one rule rather than an address with an optional token.
	//
	// ProvisionToken IS A SECRET AND NEVER REACHES A LOG LINE OR AN ERROR
	// MESSAGE. Load's refusals below name the VARIABLE and say what is wrong
	// with its value in the abstract — too short, or set without its partner —
	// and never quote a byte of it, unlike CATENARY_PORT's own refusal, which
	// echoes what it was given because a port is not a credential.
	ProvisionAddr  string
	ProvisionToken string
}

// MinProvisionTokenBytes is the floor on CATENARY_PROVISION_TOKEN.
//
// Thirty-two bytes, matching store.TokenBytes' own entropy: this credential can
// obtain an enrollment token for any existing person, which is a device
// enrolled as them, so it is the most powerful credential in the service and a
// short one is the cheapest possible way to lose everything in it. The floor is
// on LENGTH and not on entropy, because a length is the only property a config
// loader can actually check — "generated in Signet" is what supplies the
// entropy, and deploy/README.md says so.
const MinProvisionTokenBytes = 32

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
	if c.HeartbeatIntervalSec, err = positiveInt("CATENARY_HEARTBEAT_INTERVAL_SEC", os.Getenv("CATENARY_HEARTBEAT_INTERVAL_SEC")); err != nil {
		return c, err
	}
	if c.MissedPongLimit, err = positiveInt("CATENARY_MISSED_PONG_LIMIT", os.Getenv("CATENARY_MISSED_PONG_LIMIT")); err != nil {
		return c, err
	}

	// CANT-131's provisioning surface. BOTH OR NEITHER, and the half-configured
	// cases are the whole reason this is not two independent reads.
	//
	// NOT TRIMMED, AND THE TOKEN IS THE REASON. Every other variable here goes
	// through TrimSpace because a stray space in a compose file is a typo; a
	// credential is a byte string that Signet generated and Purser presents
	// verbatim, and silently trimming it here would mean the two services
	// disagree about what the secret is whenever one of them was pasted with a
	// trailing newline — a 401 an operator cannot see the cause of. So the
	// address is trimmed and the token is not: presence is decided on the raw
	// bytes, and a token that is only whitespace is caught by the length floor
	// below rather than turned into "unset", which would silently disarm the
	// surface.
	c.ProvisionAddr = strings.TrimSpace(os.Getenv("CATENARY_PROVISION_ADDR"))
	c.ProvisionToken = os.Getenv("CATENARY_PROVISION_TOKEN")
	if err := c.validateProvisioning(); err != nil {
		return c, err
	}

	return c, nil
}

// ProvisioningEnabled reports whether this process has a provisioning surface at
// all — the composition root's gate, and the one place the "both or neither" rule
// is read rather than enforced.
//
// BOTH, NOT EITHER. After Load the two cannot disagree, so this could test one of
// them; it tests both so that a Config built by hand — which a test does, and
// which bypasses Load's rules exactly as it bypasses every other rule there —
// cannot produce a listener with no credential or a credential with no listener.
func (c Config) ProvisioningEnabled() bool {
	return c.ProvisionAddr != "" && c.ProvisionToken != ""
}

// validateProvisioning enforces the three rules ruling 2 states, each naming
// its variable and none of them echoing the credential.
//
// A method on Config rather than a stanza inside Load so cmd/catenary can state
// the same rules over a Config it built by hand — and so this can be read on
// its own, which is what a reviewer of a door wants.
func (c Config) validateProvisioning() error {
	hasAddr := c.ProvisionAddr != ""
	hasToken := c.ProvisionToken != ""

	switch {
	case !hasAddr && !hasToken:
		// The surface does not exist. Nothing to check, nothing to open.
		return nil
	case hasAddr && !hasToken:
		return fmt.Errorf("config: CATENARY_PROVISION_ADDR is set without CATENARY_PROVISION_TOKEN — " +
			"the provisioning surface is both or neither, and a listener with no credential " +
			"would serve account creation to anything that can reach the port")
	case !hasAddr && hasToken:
		return fmt.Errorf("config: CATENARY_PROVISION_TOKEN is set without CATENARY_PROVISION_ADDR — " +
			"the provisioning surface is both or neither, and a credential with no listener is " +
			"a deployment that believes it has a provisioning surface and does not")
	}

	// LENGTH IN BYTES, NOT RUNES, AND THE VALUE IS NEVER QUOTED. len() on a Go
	// string is bytes, which is what an attacker guesses; the count is reported
	// because "yours is 24" is the one thing an operator needs and says nothing
	// about which bytes they are.
	if n := len(c.ProvisionToken); n < MinProvisionTokenBytes {
		return fmt.Errorf("config: CATENARY_PROVISION_TOKEN is %d bytes, and at least %d are required — "+
			"this credential can obtain an enrollment token for any person in the service; "+
			"generate it in Signet", n, MinProvisionTokenBytes)
	}

	// EXACT-STRING EQUALITY ONLY, DELIBERATELY. Two listeners on one address
	// cannot both bind, so this is already fail-closed: the second net.Listen
	// fails and the process exits. What this adds is the sentence — the
	// OBVIOUS way to get here is copying CATENARY_PORT's value into the
	// provisioning address, and "address already in use" does not name either
	// variable. Equivalent-but-differently-spelled addresses (`:4012` against
	// `0.0.0.0:4012`) are left to the bind, because a config loader that
	// resolved addresses to compare them would be doing the kernel's job
	// badly.
	if c.ProvisionAddr == c.Addr {
		return fmt.Errorf("config: CATENARY_PROVISION_ADDR is %q, the address CATENARY_PORT already "+
			"listens on — the provisioning surface is a SECOND listener serving nothing else, "+
			"and sharing the port would put account creation on the routed one", c.ProvisionAddr)
	}

	return nil
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
