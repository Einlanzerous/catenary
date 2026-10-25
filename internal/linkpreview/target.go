package linkpreview

import (
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

const maxURLBytes = 2048

// parseTarget validates and canonicalizes a URL the client sent (or a redirect
// or og:image resolved to). It is about SYNTAX and what a URL may ask for; it
// says nothing about the host, which is judged at connect time and nowhere
// else. allowPort is the port policy (80 and 443 in production).
//
// Refused: more than 2048 bytes, any scheme but http and https, userinfo, no
// host, a port outside the policy. Canonical form: lowercase scheme, IDNA-ASCII
// host, fragment dropped.
func parseTarget(raw string, allowPort func(int) bool) (*url.URL, *Error) {
	if raw == "" || len(raw) > maxURLBytes {
		return nil, newErr(ClassBadURL, "length", nil)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, newErr(ClassBadURL, "syntax", nil)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, newErr(ClassBadURL, "scheme", nil)
	}
	if u.User != nil {
		return nil, newErr(ClassBadURL, "userinfo", nil)
	}
	if u.Opaque != "" {
		return nil, newErr(ClassBadURL, "syntax", nil)
	}
	host := u.Hostname()
	if host == "" {
		return nil, newErr(ClassBadURL, "host", nil)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || !allowPort(n) {
			return nil, newErr(ClassBlocked, "port", nil)
		}
	}
	ascii := host
	if _, ipErr := netip.ParseAddr(host); ipErr != nil {
		// A name, not an address literal: IDNA to ASCII.
		var ierr error
		if ascii, ierr = idna.Lookup.ToASCII(host); ierr != nil || ascii == "" {
			return nil, newErr(ClassBadURL, "host", nil)
		}
	}
	if strings.ContainsAny(ascii, " \t\r\n") {
		return nil, newErr(ClassBadURL, "host", nil)
	}
	if strings.Contains(ascii, ":") { // an IPv6 literal goes back inside brackets
		ascii = "[" + ascii + "]"
	}
	if p := u.Port(); p != "" {
		u.Host = ascii + ":" + p
	} else {
		u.Host = ascii
	}
	u.Fragment, u.RawFragment = "", ""
	return u, nil
}

func prodPort(n int) bool { return n == 80 || n == 443 }
