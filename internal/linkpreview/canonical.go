package linkpreview

// Canonical validates a URL a client sent and returns the form Preview works
// from and the card stores: lowercase scheme, IDNA-ASCII host, no fragment.
//
// It is the endpoint's whole answer to "is this URL well formed", and it opens
// no socket. Every refusal is a *Error of class bad_url, INCLUDING a port the
// policy does not allow: that is about the syntax the client sent and says
// nothing about the network, so it can be a 400 without being an oracle (the
// wire schema promises a 400 for it). Preview itself still reports a refused
// port as blocked, because it is also the redirect check.
func Canonical(raw string) (string, error) {
	u, perr := parseTarget(raw, prodPort)
	if perr != nil {
		if perr.Class == ClassBlocked && perr.Detail == "port" {
			return "", newErr(ClassBadURL, "port", nil)
		}
		return "", perr
	}
	return u.String(), nil
}
