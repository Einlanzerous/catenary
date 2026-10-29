package linkpreview

import (
	"context"
	"errors"
	"net"
)

// Class is the coarse reason a fetch produced no card. The values are the ones
// the endpoint logs (CANT-308, fetch-policy) plus bad_url, which is the one
// the caller answers 400 for, and unavailable for "the target answered, and not
// with a page".
type Class string

const (
	ClassBadURL      Class = "bad_url"
	ClassBlocked     Class = "blocked"
	ClassTimeout     Class = "timeout"
	ClassTooLarge    Class = "too_large"
	ClassNotHTML     Class = "not_html"
	ClassEmpty       Class = "empty"
	ClassDNS         Class = "dns"
	ClassUnavailable Class = "unavailable"
)

// Error is what Preview returns. Its text is the class and a fixed detail and
// NEVER the URL, its path or its query: a logger that prints err is safe.
type Error struct {
	Class Class
	// Detail is a short fixed token. For ClassBlocked it is the address class
	// ("loopback", "private", "link_local", ...) or "redirect" or "port", which
	// is what the endpoint logs at warn. For ClassEmpty it is "", one of the
	// wall_* tokens in wall.go, or "final_host" (finalhost.go).
	Detail string
	cause  error
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return "linkpreview: " + string(e.Class)
	}
	return "linkpreview: " + string(e.Class) + " (" + e.Detail + ")"
}

// Unwrap exposes the cause for errors.Is and errors.As only. Do not log it: a
// *url.Error in the chain carries the URL.
func (e *Error) Unwrap() error { return e.cause }

func newErr(c Class, detail string, cause error) *Error {
	return &Error{Class: c, Detail: detail, cause: cause}
}

// ClassOf returns the class of an error from Preview, ClassUnavailable for
// anything that is not one of ours.
func ClassOf(err error) Class {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ClassUnavailable
}

// DetailOf returns the Error's detail token, or "".
func DetailOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Detail
	}
	return ""
}

// blockedError is what the dialer's Control function returns for a refused
// socket. It survives the net.OpError and url.Error wrappers, so classify can
// find it with errors.As.
type blockedError struct{ detail string }

func (b *blockedError) Error() string { return "address refused: " + b.detail }

// classify maps whatever the HTTP client returned onto a *Error. It looks at the
// error chain, never at the message, because a message carries the URL.
func classify(err error) *Error {
	if err == nil {
		return nil
	}
	var ours *Error
	if errors.As(err, &ours) {
		return ours
	}
	var b *blockedError
	if errors.As(err, &b) {
		return newErr(ClassBlocked, b.detail, err)
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsTimeout {
			return newErr(ClassTimeout, "dns", err)
		}
		return newErr(ClassDNS, "", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return newErr(ClassTimeout, "", err)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return newErr(ClassTimeout, "", err)
	}
	return newErr(ClassUnavailable, "", err)
}

// NewError builds the error a Previewer other than Guarded returns, so a second
// implementation (IDEA-66's unfurl service) or a test double reports the same
// classes the endpoint logs. detail follows the conventions on Error.Detail and
// must never contain the URL.
func NewError(c Class, detail string) error { return newErr(c, detail, nil) }
