package linkpreview

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
)

// Guarded is the in-process Previewer: it fetches with a client whose every
// socket passes the address policy at connect time. See the package comment.
type Guarded struct {
	cfg    Config
	env    environment
	client *http.Client
}

var _ Previewer = (*Guarded)(nil)

// environment is the three things a test replaces and production never does:
// name resolution, the address policy and the port policy. They are fields of
// an unexported type so that no caller outside this package can weaken the guard
// by configuration.
type environment struct {
	lookup    func(ctx context.Context, host string) ([]netip.Addr, error)
	judge     func(netip.Addr) (class string, ok bool)
	allowPort func(int) bool
}

func productionEnvironment() environment {
	return environment{
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		judge:     judge,
		allowPort: prodPort,
	}
}

// New returns the production fetcher.
func New(cfg Config) *Guarded { return newGuarded(cfg, productionEnvironment()) }

func newGuarded(cfg Config, env environment) *Guarded {
	g := &Guarded{cfg: cfg, env: env}
	g.client = &http.Client{
		// No jar: no cookie is ever stored or sent.
		Jar:           nil,
		CheckRedirect: g.checkRedirect,
		Transport: &http.Transport{
			// Proxy is nil on purpose: HTTP_PROXY in the environment must not be
			// able to route a request around the dialer.
			Proxy:       nil,
			DialContext: g.dial,
			// No connection survives a policy decision, and none is reused for
			// another hop.
			DisableKeepAlives: true,
			// We set Accept-Encoding ourselves, so the transport would hand us the
			// compressed bytes anyway; turning this off keeps that explicit. The
			// decompression, and the cap on its OUTPUT, are ours (readPage).
			DisableCompression:     true,
			TLSHandshakeTimeout:    cfg.ConnectTimeout,
			ResponseHeaderTimeout:  cfg.PageTimeout,
			MaxResponseHeaderBytes: 64 << 10,
			MaxIdleConns:           -1,
		},
	}
	return g
}

// dial is the only way this client reaches the network. It resolves the host
// itself so a test can substitute the resolver, then connects to each answer
// through a net.Dialer whose Control function judges the address the kernel is
// about to connect to. The judgment is NOT made on the resolver's answer: a
// second resolution, a redirect, a mapped address and a literal all arrive at
// the same Control call.
func (g *Guarded) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, &blockedError{detail: "network"}
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = g.env.lookup(ctx, host)
		if err != nil {
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) {
				return nil, err
			}
			return nil, &net.DNSError{Err: "lookup failed", Name: "host", IsNotFound: true}
		}
		if len(ips) == 0 {
			return nil, &net.DNSError{Err: "no addresses", Name: "host", IsNotFound: true}
		}
	}

	d := net.Dialer{
		Timeout:   g.cfg.ConnectTimeout,
		KeepAlive: -1,
		ControlContext: func(_ context.Context, network, address string, _ syscall.RawConn) error {
			return g.control(network, address)
		},
	}
	var lastErr error
	var blocked *blockedError
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		var b *blockedError
		if errors.As(err, &b) {
			if blocked == nil {
				blocked = b
			}
			continue
		}
		lastErr = err
	}
	// A name with one allowed and one refused answer still connects to the allowed
	// one above. If every attempt was refused, say so; a refusal outranks an
	// ordinary failure only when nothing else went wrong.
	if lastErr == nil && blocked != nil {
		return nil, blocked
	}
	return nil, lastErr
}

// control judges the remote address of one socket. Anything it cannot parse is
// refused: failing open here is the SSRF.
func (g *Guarded) control(network, address string) error {
	if network != "tcp4" && network != "tcp6" {
		return &blockedError{detail: "network"}
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return &blockedError{detail: "unparseable"}
	}
	if !g.env.allowPort(int(ap.Port())) {
		return &blockedError{detail: "port"}
	}
	if class, ok := g.env.judge(ap.Addr()); !ok {
		return &blockedError{detail: class}
	}
	return nil
}

// checkRedirect is the redirect policy. Every hop is then dialed through the
// same guarded dialer, so a redirect to http://169.254.169.254/ or to a name
// that now resolves privately fails exactly as the first request would.
func (g *Guarded) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > g.cfg.MaxRedirects {
		return newErr(ClassBlocked, "redirect", nil)
	}
	if _, perr := parseTarget(req.URL.String(), g.env.allowPort); perr != nil {
		return newErr(ClassBlocked, "redirect", nil)
	}
	if prev := via[len(via)-1]; prev.URL.Scheme == "https" && req.URL.Scheme == "http" {
		return newErr(ClassBlocked, "downgrade", nil)
	}
	// net/http adds a Referer on a redirect it follows. We send none.
	req.Header.Del("Referer")
	return nil
}

// Preview fetches rawURL and returns its card. See Previewer.
func (g *Guarded) Preview(ctx context.Context, rawURL string) (*Preview, error) {
	u, perr := parseTarget(rawURL, g.env.allowPort)
	if perr != nil {
		return nil, perr
	}
	ctx, cancel := context.WithTimeout(ctx, g.cfg.TotalTimeout)
	defer cancel()

	m, finalURL, err := g.fetchPage(ctx, u)
	if err != nil {
		return nil, err
	}
	p := &Preview{
		URL:         u.String(),
		Title:       clean(m.title, maxTitleRunes),
		Description: clean(m.description, maxDescriptionRunes),
		SiteName:    clean(m.siteName, maxSiteNameRunes),
	}
	if strings.TrimSpace(m.image) != "" {
		img, class := g.fetchImage(ctx, finalURL, strings.TrimSpace(m.image))
		p.Image, p.ImageClass = img, class
	}
	// A page with neither a title nor an image claims nothing worth a card.
	if p.Title == "" && len(p.Image) == 0 {
		return nil, newErr(ClassEmpty, "", nil)
	}
	return p, nil
}

func (g *Guarded) get(ctx context.Context, u *url.URL, accept, acceptEncoding string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, newErr(ClassBadURL, "syntax", nil)
	}
	// What is sent, and all of it: no cookies, no Referer, no Authorization.
	req.Header = http.Header{
		"User-Agent":      {g.cfg.UserAgent},
		"Accept":          {accept},
		"Accept-Encoding": {acceptEncoding},
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, classify(err)
	}
	return resp, nil
}

// fetchPage returns the metadata of the page and the URL it finally came from
// (after redirects), which is the base an og:image is resolved against.
func (g *Guarded) fetchPage(ctx context.Context, u *url.URL) (meta, *url.URL, error) {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.PageTimeout)
	defer cancel()

	resp, err := g.get(ctx, u, "text/html", "gzip")
	if err != nil {
		return meta{}, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return meta{}, nil, newErr(ClassUnavailable, "status", nil)
	}
	// The type is decided from the header BEFORE one byte of body is read.
	mt, _, merr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if merr != nil || (mt != "text/html" && mt != "application/xhtml+xml") {
		return meta{}, nil, newErr(ClassNotHTML, "", nil)
	}

	var body io.Reader
	// The wire is bounded too: gzip of at most MaxPageBytes of output is not much
	// bigger than that, and anything beyond is cut.
	wire := io.LimitReader(resp.Body, g.cfg.MaxPageBytes+(64<<10))
	switch enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
		body = wire
	case "gzip", "x-gzip":
		// The transport does not decompress (we asked for gzip by hand), so the
		// reader is ours, and THE CAP APPLIES TO ITS OUTPUT below: a few kilobytes
		// of gzip can inflate to gigabytes.
		zr, zerr := gzip.NewReader(wire)
		if zerr != nil {
			return meta{}, nil, newErr(ClassUnavailable, "gzip", nil)
		}
		defer zr.Close()
		body = zr
	default:
		return meta{}, nil, newErr(ClassNotHTML, "encoding", nil)
	}

	capped := &capReader{r: body, left: g.cfg.MaxPageBytes}
	m := extract(capped)
	if (capped.cut || m.overflow) && m.title == "" && m.image == "" {
		return meta{}, nil, newErr(ClassTooLarge, "", nil)
	}
	if err := ctx.Err(); err != nil && m.title == "" && m.image == "" {
		return meta{}, nil, classify(err)
	}
	return m, resp.Request.URL, nil
}

// fetchImage returns the image bytes, or nil and the class of the reason it
// could not. A failed image costs the card its picture and never the card.
func (g *Guarded) fetchImage(ctx context.Context, base *url.URL, ref string) ([]byte, Class) {
	ref = strings.TrimSpace(ref)
	r, err := url.Parse(ref)
	if err != nil {
		return nil, ClassBadURL
	}
	abs, perr := parseTarget(base.ResolveReference(r).String(), g.env.allowPort)
	if perr != nil {
		return nil, perr.Class
	}

	ctx, cancel := context.WithTimeout(ctx, g.cfg.ImageTimeout)
	defer cancel()
	resp, gerr := g.get(ctx, abs, "image/*", "identity")
	if gerr != nil {
		return nil, ClassOf(gerr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ClassUnavailable
	}
	// The type is NOT decided here: media.IngestImage sniffs the bytes and
	// ignores what the target called them.
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, g.cfg.MaxImageBytes+1))
	if rerr != nil {
		return nil, ClassOf(classify(rerr))
	}
	if int64(len(data)) > g.cfg.MaxImageBytes {
		return nil, ClassTooLarge
	}
	if len(data) == 0 {
		return nil, ClassEmpty
	}
	return data, ""
}

// capReader passes through at most `left` bytes and then reports EOF, recording
// that it cut.
type capReader struct {
	r    io.Reader
	left int64
	cut  bool
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		// One more byte decides whether this was exactly the end or a cut.
		var one [1]byte
		if n, _ := c.r.Read(one[:]); n > 0 {
			c.cut = true
		}
		return 0, io.EOF
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}
