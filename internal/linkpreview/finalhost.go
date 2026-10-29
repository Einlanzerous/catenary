package linkpreview

// CANT-337 (CANT-331 ruling 0 option A, ruling 1 option A): the final host. A
// shortener's card carries a title and a picture the TARGET's site chose, so
// when the fetch's redirects left the pasted site the card names the host that
// actually served the page (wire LinkAttachment.final_host), and a client shows
// it after url's own host. When every hop stayed on the pasted site it is
// absent, so its presence is the whole instruction to a client.
//
// The value is page-influenced (the redirect chain is the target's choice), so
// it is bounded to a bare host here, by the same pattern the database CHECK
// (migration 0015) and the three generated decoders hold it to. A final host
// that still does not match is no card: a card that cannot name who served it
// does not ship, rather than shipping one that hides a hop.
//
// Same site is decided once, here, at fetch time, and frozen with the card: the
// registrable domain (eTLD+1) from the public suffix list compiled into
// golang.org/x/net/publicsuffix, so no client carries a list or a rule. Where
// there is no registrable domain (an address literal, or a host that is itself
// a public suffix), the two hosts are compared whole after dropping a leading
// "www.".

import (
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// detailFinalHost is the ClassEmpty detail for a final host the pattern refuses.
const detailFinalHost = "final_host"

// finalHostPattern and finalHostMaxLen are LinkAttachment.final_host's schema
// constraints (schema/catenary.wire.v1.schema.json), copied, not generated, as
// migration 0015's CHECK copies them too; TestFinalHostPatternIsTheSchemas reads
// the schema file and fails when the two differ.
var finalHostPattern = regexp.MustCompile(`^(?:(?:[a-z0-9_-]{1,63}\.)+[a-z0-9_-]{1,63}|\[[0-9a-f:.]+\])$`)

const finalHostMaxLen = 253

// finalHost returns the host to store as the card's final host: "" when the
// final URL is on the pasted URL's site (including when nothing redirected),
// the final URL's canonical host otherwise. ok is false only when there is a
// host to store and it does not match the wire pattern, which the caller turns
// into no card. A host that is never stored is never held to the pattern, so a
// fetch that stayed on its site is a card exactly as it was before this field.
func finalHost(pasted, final *url.URL) (host string, ok bool) {
	from, okFrom := canonicalHost(pasted)
	to, okTo := canonicalHost(final)
	if !okTo {
		return "", false
	}
	if okFrom && sameSite(from, to) {
		return "", true
	}
	if len(to) > finalHostMaxLen || !finalHostPattern.MatchString(to) {
		return "", false
	}
	return to, true
}

// canonicalHost is a URL's host in final_host's form: run through parseTarget
// (IDNA to ASCII for a name, which also lowercases), the port and a trailing dot
// dropped, an address literal rewritten by netip (lowercase, compressed) with an
// IPv6 one kept in brackets. It does not apply the pattern (finalHost does, to
// the one host it stores). The port policy is not this function's question (the
// dialer and checkRedirect already applied it), so any port is accepted and
// discarded. ok is false only when parseTarget refuses the URL.
func canonicalHost(u *url.URL) (string, bool) {
	c, perr := parseTarget(u.String(), func(int) bool { return true })
	if perr != nil {
		return "", false
	}
	host := strings.TrimSuffix(c.Hostname(), ".")
	if a, err := netip.ParseAddr(host); err == nil {
		host = a.String()
		if a.Is6() {
			host = "[" + host + "]"
		}
	}
	return host, true
}

// sameSite reports whether two canonical hosts are one site: equal registrable
// domains, or, where either has none, equal hosts after a leading "www.".
func sameSite(a, b string) bool {
	if ra, rb := registrable(a), registrable(b); ra != "" && rb != "" {
		return ra == rb
	}
	return strings.TrimPrefix(a, "www.") == strings.TrimPrefix(b, "www.")
}

// registrable is a host's eTLD+1, or "" when it has none. An address literal
// has none by definition. publicsuffix (x/net v0.58.0) already refuses a bare
// literal, but it would read the bracketed IPv6 form canonicalHost produces as
// a name, and this keeps the refusal from depending on the library's version.
func registrable(host string) string {
	if strings.HasPrefix(host, "[") {
		return ""
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return ""
	}
	r, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return r
}
