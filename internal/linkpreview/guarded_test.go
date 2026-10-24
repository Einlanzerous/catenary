package linkpreview

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// NOTHING IN THIS FILE TOUCHES THE INTERNET. Every target is an httptest server
// on 127.0.0.1 or a socket that is refused before it connects, and names resolve
// through a double. The production policy is exercised for real where it can be
// (a loopback listener the production guard must refuse); where a test needs a
// "public" address it has to be one it can actually reach, so a test
// environment declares 127.0.0.1 public and leaves everything else to the real
// judge.

func testConfig() Config {
	c := DefaultConfig()
	c.ConnectTimeout = time.Second
	c.PageTimeout = 3 * time.Second
	c.ImageTimeout = 2 * time.Second
	c.TotalTimeout = 5 * time.Second
	return c
}

// testEnv treats the listed addresses as public and every other address by the
// production judge, with any port allowed (httptest picks its own) and names
// answered from answers (a host -> a list of answers, consumed in order, the
// last one repeating).
func testEnv(public []string, answers map[string][][]string) (environment, *lookups) {
	pub := map[netip.Addr]bool{}
	for _, p := range public {
		pub[netip.MustParseAddr(p)] = true
	}
	l := &lookups{answers: answers, count: map[string]int{}}
	return environment{
		lookup: l.lookup,
		judge: func(a netip.Addr) (string, bool) {
			if pub[a] {
				return "", true
			}
			return judge(a)
		},
		allowPort: func(int) bool { return true },
	}, l
}

type lookups struct {
	mu      sync.Mutex
	answers map[string][][]string
	count   map[string]int
}

func (l *lookups) lookup(_ context.Context, host string) ([]netip.Addr, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	seq, ok := l.answers[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	i := l.count[host]
	l.count[host]++
	if i >= len(seq) {
		i = len(seq) - 1
	}
	var out []netip.Addr
	for _, s := range seq[i] {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

func loopbackGuard(t *testing.T) *Guarded {
	t.Helper()
	env, _ := testEnv([]string{"127.0.0.1"}, nil)
	return newGuarded(testConfig(), env)
}

const headHTML = `<!doctype html><html><head><meta charset="utf-8">
<title>Plain title</title>
<meta property="og:title" content="OG title">
<meta property="og:description" content="An OG description">
<meta property="og:site_name" content="Example Site">
<meta property="og:image" content="/card.png">
</head><body><p>hello</p></body></html>`

// a 1x1 PNG; the fetcher never decodes it, so any bytes would do, but real ones
// keep the fixture honest.
var tinyPNG = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89}

func serve(t *testing.T, h http.Handler) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, srv.URL
}

func htmlHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/card.png" {
			w.Header().Set("Content-Type", "image/png")
			w.Write(tinyPNG)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	}
}

func TestPreviewHappyPath(t *testing.T) {
	_, base := serve(t, htmlHandler(headHTML))
	p, err := loopbackGuard(t).Preview(context.Background(), base+"/page#frag")
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "OG title" || p.Description != "An OG description" || p.SiteName != "Example Site" {
		t.Errorf("fields = %q / %q / %q", p.Title, p.Description, p.SiteName)
	}
	if !bytes.Equal(p.Image, tinyPNG) || p.ImageClass != "" {
		t.Errorf("image = %d bytes, class %q", len(p.Image), p.ImageClass)
	}
	if strings.Contains(p.URL, "#") {
		t.Errorf("fragment kept: %s", p.URL)
	}
}

func TestPrecedenceAndFallbacks(t *testing.T) {
	cases := []struct {
		name, html, title, desc string
	}{
		{"title tag only", `<head><title> Just   a title </title></head>`, "Just a title", ""},
		{"twitter over title tag", `<head><title>T</title><meta name="twitter:title" content="TW"><meta name="twitter:description" content="TWD"></head>`, "TW", "TWD"},
		{"og over twitter", `<head><meta name="twitter:title" content="TW"><meta property="og:title" content="OG"></head>`, "OG", ""},
		{"plain description", `<head><title>T</title><meta name="description" content="plain"></head>`, "T", "plain"},
		{"first og:title wins", `<head><meta property="og:title" content="one"><meta property="og:title" content="two"></head>`, "one", ""},
		{"entities decoded", `<head><meta property="og:title" content="Tom &amp; Jerry &#8212; ok"></head>`, "Tom & Jerry \u2014 ok", ""},
		{"stops at body", `<head><title>A</title></head><body><meta property="og:title" content="late"></body>`, "A", ""},
		{"stops at first body even without head end", `<title>A</title><body><meta property="og:title" content="late">`, "A", ""},
	}
	for _, c := range cases {
		_, base := serve(t, htmlHandler(c.html))
		p, err := loopbackGuard(t).Preview(context.Background(), base+"/")
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if p.Title != c.title || p.Description != c.desc {
			t.Errorf("%s: title %q desc %q, want %q %q", c.name, p.Title, p.Description, c.title, c.desc)
		}
	}
}

func TestEmptyPageIsNoCard(t *testing.T) {
	for name, body := range map[string]string{
		"nothing":           `<html><head></head><body>x</body></html>`,
		"description only":  `<head><meta property="og:description" content="d"></head>`,
		"blank title":       `<head><title>   </title></head>`,
		"title is controls": "<head><meta property=\"og:title\" content=\"\u202e\u202c\u0007\"></head>",
	} {
		_, base := serve(t, htmlHandler(body))
		_, err := loopbackGuard(t).Preview(context.Background(), base+"/")
		if ClassOf(err) != ClassEmpty {
			t.Errorf("%s: err = %v, want empty", name, err)
		}
	}
}

// A page with an image and no title is still a card.
func TestImageAloneIsACard(t *testing.T) {
	_, base := serve(t, htmlHandler(`<head><meta property="og:image" content="/card.png"></head>`))
	p, err := loopbackGuard(t).Preview(context.Background(), base+"/")
	if err != nil || len(p.Image) == 0 || p.Title != "" {
		t.Fatalf("p = %+v, err = %v", p, err)
	}
}

// The production policy, for real: a listener on loopback that the production
// guard must refuse at dial, and which must never see a request.
func TestProductionGuardRefusesLoopbackAtDial(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, headHTML)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	// The test server's port is not 80 or 443, so make the port policy pass and
	// leave the ADDRESS policy exactly as production has it.
	env := productionEnvironment()
	env.allowPort = func(int) bool { return true }
	g := newGuarded(testConfig(), env)

	for _, host := range []string{u.Host, "localhost:" + u.Port()} {
		_, err := g.Preview(context.Background(), "http://"+host+"/")
		if ClassOf(err) != ClassBlocked || DetailOf(err) != "loopback" {
			t.Errorf("%s: err = %v, want blocked (loopback)", host, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the listener saw %d request(s); the guard must refuse before connecting", hits.Load())
	}

	// And New (the production constructor) refuses it too, on the port policy first.
	if _, err := New(testConfig()).Preview(context.Background(), srv.URL+"/"); ClassOf(err) != ClassBlocked || DetailOf(err) != "port" {
		t.Errorf("New: err = %v, want blocked (port)", err)
	}
}

func TestProductionGuardLiteralsAndMappedAddresses(t *testing.T) {
	env := productionEnvironment()
	env.allowPort = func(int) bool { return true }
	g := newGuarded(testConfig(), env)
	for _, host := range []string{
		"127.0.0.1:9", "10.0.0.1:9", "192.168.1.1:9", "169.254.169.254:9", "100.64.0.1:9",
		"[::1]:9", "[::ffff:127.0.0.1]:9", "[::ffff:169.254.169.254]:9", "[64:ff9b::7f00:1]:9",
		"[2002:7f00:1::]:9", "[fe80::1]:9", "[fd00::1]:9", "0.0.0.0:9", "224.0.0.1:9",
	} {
		_, err := g.Preview(context.Background(), "http://"+host+"/")
		if ClassOf(err) != ClassBlocked {
			t.Errorf("%s: err = %v, want blocked", host, err)
		}
	}
}

// DNS rebinding. The name answers a public address first and a private one
// after. There is no check-then-connect to race: the second connection is judged
// on the address it is actually going to.
func TestRebindingResolverIsRefusedAtDial(t *testing.T) {
	var hits atomic.Int32
	srv, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<head><title>ok</title></head>`)
	}))
	_ = srv
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(base, "http://"))

	// "Public" is 127.0.0.1 here (it is the one we can listen on); the private
	// answer is 127.0.0.2, which the real judge calls loopback.
	env, l := testEnv([]string{"127.0.0.1"}, map[string][][]string{
		"rebind.test": {{"127.0.0.1"}, {"127.0.0.2"}},
	})
	g := newGuarded(testConfig(), env)
	target := "http://rebind.test:" + port + "/"

	if _, err := g.Preview(context.Background(), target); err != nil {
		t.Fatalf("first resolution (public) should fetch: %v", err)
	}
	before := hits.Load()
	_, err := g.Preview(context.Background(), target)
	if ClassOf(err) != ClassBlocked || DetailOf(err) != "loopback" {
		t.Fatalf("second resolution (private): err = %v, want blocked", err)
	}
	if hits.Load() != before {
		t.Fatal("the target saw a request after rebinding to a private address")
	}
	if l.count["rebind.test"] != 2 {
		t.Fatalf("lookups = %d", l.count["rebind.test"])
	}
}

// The same attack through a redirect: the first hop is allowed, the Location
// names a host that now resolves privately.
func TestRedirectToRebindingNameIsRefused(t *testing.T) {
	var hits atomic.Int32
	_, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "http://rebind.test:"+portOf(r.Host)+"/next", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<head><title>reached</title></head>`)
	}))
	env, _ := testEnv([]string{"127.0.0.1"}, map[string][][]string{
		"rebind.test": {{"127.0.0.2"}},
	})
	g := newGuarded(testConfig(), env)
	_, err := g.Preview(context.Background(), base+"/start")
	if ClassOf(err) != ClassBlocked {
		t.Fatalf("err = %v, want blocked", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want only the first hop", hits.Load())
	}
}

func portOf(hostport string) string {
	_, p, _ := net.SplitHostPort(hostport)
	return p
}

func TestRedirectToPrivateAddressIsRefused(t *testing.T) {
	for _, loc := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/",
		"http://[::ffff:169.254.169.254]/",
		"http://127.0.0.2/",
	} {
		_, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, loc, http.StatusMovedPermanently)
		}))
		_, err := loopbackGuard(t).Preview(context.Background(), base+"/")
		if ClassOf(err) != ClassBlocked {
			t.Errorf("%s: err = %v, want blocked", loc, err)
		}
	}
}

func TestRedirectLimit(t *testing.T) {
	chain := func(n int) string {
		_, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var i int
			fmt.Sscanf(r.URL.Path, "/r%d", &i)
			if i < n {
				http.Redirect(w, r, fmt.Sprintf("/r%d", i+1), http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<head><title>end</title></head>`)
		}))
		return base + "/r0"
	}
	if p, err := loopbackGuard(t).Preview(context.Background(), chain(3)); err != nil || p.Title != "end" {
		t.Fatalf("3 redirects: %v %+v", err, p)
	}
	_, err := loopbackGuard(t).Preview(context.Background(), chain(4))
	if ClassOf(err) != ClassBlocked || DetailOf(err) != "redirect" {
		t.Fatalf("4 redirects: err = %v, want blocked (redirect)", err)
	}
}

func TestCheckRedirectPolicy(t *testing.T) {
	g := loopbackGuard(t)
	req := func(raw string) *http.Request {
		r, _ := http.NewRequest("GET", raw, nil)
		return r
	}
	via := func(raw string) []*http.Request { return []*http.Request{req(raw)} }

	if err := g.checkRedirect(req("https://example.com/b"), via("https://example.com/a")); err != nil {
		t.Errorf("https->https refused: %v", err)
	}
	if err := g.checkRedirect(req("https://example.com/b"), via("http://example.com/a")); err != nil {
		t.Errorf("http->https refused: %v", err)
	}
	for name, c := range map[string]struct{ to, from string }{
		"downgrade":  {"http://example.com/b", "https://example.com/a"},
		"scheme":     {"ftp://example.com/b", "https://example.com/a"},
		"file":       {"file:///etc/passwd", "https://example.com/a"},
		"userinfo":   {"https://u:p@example.com/b", "https://example.com/a"},
		"to no host": {"https:///b", "https://example.com/a"},
	} {
		if err := g.checkRedirect(req(c.to), via(c.from)); ClassOf(err) != ClassBlocked {
			t.Errorf("%s: err = %v, want blocked", name, err)
		}
	}

	// A port outside the policy is refused with the PRODUCTION port rule.
	pg := New(testConfig())
	if err := pg.checkRedirect(req("http://example.com:8080/"), via("http://example.com/")); ClassOf(err) != ClassBlocked {
		t.Errorf("port 8080: err = %v, want blocked", err)
	}
	if err := pg.checkRedirect(req("http://example.com:80/"), via("http://example.com/")); err != nil {
		t.Errorf("port 80 refused: %v", err)
	}
}

func gz(t *testing.T, parts ...[]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	for _, p := range parts {
		if _, err := w.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func gzipHandler(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			http.Error(w, "need gzip", 400)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(body)
	}
}

// A gzip bomb: a few KB on the wire, hundreds of MB inflated. The cap is on the
// decompressed output, so the page is cut at 2 MiB however small it traveled.
func TestGzipBombIsCutAtTheCap(t *testing.T) {
	// Small tokens, so it is the 2 MiB cap that ends the read and not the
	// tokenizer's own limit on one token.
	chunk := bytes.Repeat([]byte("<i>x</i>"), 1<<17) // 1 MiB
	parts := [][]byte{[]byte("<html><head>")}
	for i := 0; i < 64; i++ { // 64 MiB inflated
		parts = append(parts, chunk)
	}
	parts = append(parts, []byte(`<title>too late</title></head>`))
	bomb := gz(t, parts...)
	if len(bomb) > 1<<20 {
		t.Fatalf("fixture is not a bomb: %d bytes on the wire", len(bomb))
	}
	_, base := serve(t, gzipHandler(bomb))

	_, err := loopbackGuard(t).Preview(context.Background(), base+"/")
	if ClassOf(err) != ClassTooLarge {
		t.Fatalf("err = %v, want too_large (the title is past the cap)", err)
	}
}

// What was read before the cut still counts: the cap trims the page, it does
// not condemn it.
func TestGzipCutKeepsTagsBeforeTheCap(t *testing.T) {
	body := gz(t, []byte(`<html><head><title>early</title>`), bytes.Repeat([]byte("x"), 8<<20))
	_, base := serve(t, gzipHandler(body))
	p, err := loopbackGuard(t).Preview(context.Background(), base+"/")
	if err != nil || p.Title != "early" {
		t.Fatalf("p = %+v, err = %v", p, err)
	}
}

// The reader that hands the parser its bytes never yields more than the cap.
func TestCapReaderBoundsOutput(t *testing.T) {
	c := &capReader{r: bytes.NewReader(make([]byte, 5000)), left: 1000}
	var total int
	buf := make([]byte, 333)
	for {
		n, err := c.Read(buf)
		total += n
		if err != nil {
			break
		}
	}
	if total != 1000 || !c.cut {
		t.Fatalf("read %d bytes, cut = %v", total, c.cut)
	}
	exact := &capReader{r: bytes.NewReader(make([]byte, 1000)), left: 1000}
	for {
		if _, err := exact.Read(buf); err != nil {
			break
		}
	}
	if exact.cut {
		t.Fatal("a body of exactly the cap is not cut")
	}
}

func TestUnsupportedContentEncodingIsRefused(t *testing.T) {
	_, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "br")
		fmt.Fprint(w, "<head><title>x</title></head>")
	}))
	if _, err := loopbackGuard(t).Preview(context.Background(), base+"/"); ClassOf(err) != ClassNotHTML {
		t.Fatalf("err = %v", err)
	}
}

// The type is decided from the header before a byte of body is read: the
// handler sends headers and then WAITS, and the fetcher must answer anyway.
func TestNonHTMLIsRefusedBeforeTheBodyIsRead(t *testing.T) {
	for _, ct := range []string{"application/json", "image/png", "application/pdf", "text/plain", "", "application/octet-stream"} {
		release := make(chan struct{})
		_, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			} else {
				w.Header()["Content-Type"] = nil // suppress sniffing
			}
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			select { // never sends a body until the test is over
			case <-release:
			case <-r.Context().Done():
			}
		}))
		done := make(chan error, 1)
		go func() {
			_, err := loopbackGuard(t).Preview(context.Background(), base+"/")
			done <- err
		}()
		select {
		case err := <-done:
			if ClassOf(err) != ClassNotHTML {
				t.Errorf("%q: err = %v, want not_html", ct, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%q: still waiting for a body; the type was not decided from the header", ct)
		}
		close(release)
	}
}

func TestXHTMLIsAccepted(t *testing.T) {
	_, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xhtml+xml; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>x</title></head></html>`)
	}))
	if p, err := loopbackGuard(t).Preview(context.Background(), base+"/"); err != nil || p.Title != "x" {
		t.Fatalf("p = %+v, err = %v", p, err)
	}
}

func TestNon200IsUnavailable(t *testing.T) {
	for _, code := range []int{204, 301 /* no Location */, 403, 404, 500} {
		_, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(code)
			fmt.Fprint(w, `<head><title>nope</title></head>`)
		}))
		if _, err := loopbackGuard(t).Preview(context.Background(), base+"/"); err == nil {
			t.Errorf("%d: a card was produced", code)
		}
	}
}

// A fixture shaped like the measured YouTube page (CANT-308): 1.4 MB, og:title
// at byte ~770,876, og:image at ~770,981, </head> at ~779,037. A 512 KiB cap
// would have produced no card for the ticket's own example.
func youtubeShaped() []byte {
	var b bytes.Buffer
	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	filler := `<script nonce="x">var ytcfg={"a":"` + strings.Repeat("0123456789abcdef", 64) + `"};</script>` + "\n"
	for b.Len() < 770_000 {
		b.WriteString(filler)
	}
	b.WriteString(`<meta property="og:title" content="Rick Astley - Never Gonna Give You Up (Official Video)">`)
	b.WriteString(`<meta property="og:image" content="/card.png">`)
	b.WriteString(`<meta property="og:site_name" content="YouTube">`)
	for b.Len() < 779_000 {
		b.WriteString(filler)
	}
	b.WriteString(`</head><body>`)
	for b.Len() < 1_376_000 {
		b.WriteString(`<div class="x">` + strings.Repeat("y", 512) + `</div>`)
	}
	b.WriteString(`</body></html>`)
	return b.Bytes()
}

func TestYouTubeShapedPageYieldsACard(t *testing.T) {
	page := youtubeShaped()
	if i := bytes.Index(page, []byte("og:title")); i < 760_000 || i > 790_000 {
		t.Fatalf("fixture has og:title at %d", i)
	}
	handler := func(compress bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/card.png" {
				w.Header().Set("Content-Type", "image/png")
				w.Write(tinyPNG)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if compress {
				w.Header().Set("Content-Encoding", "gzip")
				w.Write(gz(t, page))
				return
			}
			w.Write(page)
		}
	}
	for _, compress := range []bool{false, true} {
		_, base := serve(t, handler(compress))
		p, err := loopbackGuard(t).Preview(context.Background(), base+"/watch?v=dQw4w9WgXcQ")
		if err != nil {
			t.Fatalf("gzip=%v: %v", compress, err)
		}
		if p.Title != "Rick Astley - Never Gonna Give You Up (Official Video)" || p.SiteName != "YouTube" || !bytes.Equal(p.Image, tinyPNG) {
			t.Errorf("gzip=%v: %+v", compress, p)
		}
	}
}

// With the default cap, a page whose tags sit past it yields no card.
func TestTagsPastTheCapYieldNoCard(t *testing.T) {
	late := append(bytes.Repeat([]byte(" "), 2<<20+10), []byte(`<title>late</title>`)...)
	_, base := serve(t, htmlHandler("<head>"+string(late)))
	if _, err := loopbackGuard(t).Preview(context.Background(), base+"/"); ClassOf(err) != ClassTooLarge {
		t.Fatalf("err = %v, want too_large", err)
	}
}

func TestRequestHygiene(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	_, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		switch r.URL.Path {
		case "/a":
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "1"})
			http.Redirect(w, r, "/b", http.StatusFound)
		case "/card.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(tinyPNG)
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, headHTML)
		}
	}))
	if _, err := loopbackGuard(t).Preview(context.Background(), base+"/a"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatalf("requests = %d, want 3 (page, redirect hop, image)", len(seen))
	}
	for i, h := range seen {
		if h.Get("User-Agent") != "Catenary-LinkPreview/1" {
			t.Errorf("request %d: User-Agent %q", i, h.Get("User-Agent"))
		}
		for _, bad := range []string{"Referer", "Cookie", "Authorization", "Proxy-Authorization", "Origin"} {
			if v := h.Get(bad); v != "" {
				t.Errorf("request %d sent %s: %q", i, bad, v)
			}
		}
	}
	if seen[0].Get("Accept") != "text/html" || seen[0].Get("Accept-Encoding") != "gzip" {
		t.Errorf("page request: Accept %q Accept-Encoding %q", seen[0].Get("Accept"), seen[0].Get("Accept-Encoding"))
	}
	if seen[2].Get("Accept-Encoding") != "identity" {
		t.Errorf("image request Accept-Encoding %q", seen[2].Get("Accept-Encoding"))
	}
}

func TestTransportIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	g := New(DefaultConfig())
	tr := g.client.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Fatal("Proxy is set; HTTP_PROXY could route around the dialer")
	}
	if !tr.DisableKeepAlives {
		t.Fatal("keep-alives are on; a connection could outlive a policy decision")
	}
	if g.client.Jar != nil {
		t.Fatal("a cookie jar is set")
	}
}

func TestImageFailuresCostTheImageNotTheCard(t *testing.T) {
	page := `<head><title>T</title><meta property="og:image" content="%s"></head>`
	cases := []struct {
		name  string
		ref   string
		serve func(w http.ResponseWriter)
		class Class
	}{
		{"private address", "http://169.254.169.254/x.png", nil, ClassBlocked},
		{"bad scheme", "javascript:alert(1)", nil, ClassBadURL},
		{"too big", "/big.png", func(w http.ResponseWriter) { w.Write(make([]byte, 5<<20+1)) }, ClassTooLarge},
		{"404", "/nope.png", func(w http.ResponseWriter) { w.WriteHeader(404) }, ClassUnavailable},
		{"empty", "/empty.png", func(w http.ResponseWriter) {}, ClassEmpty},
	}
	for _, c := range cases {
		_, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ".png") && c.serve != nil {
				c.serve(w)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, page, c.ref)
		}))
		p, err := loopbackGuard(t).Preview(context.Background(), base+"/")
		if err != nil {
			t.Errorf("%s: card lost: %v", c.name, err)
			continue
		}
		if p.Title != "T" || p.Image != nil || p.ImageClass != c.class {
			t.Errorf("%s: title %q image %d bytes class %q, want class %q", c.name, p.Title, len(p.Image), p.ImageClass, c.class)
		}
	}
}

func TestImageAtExactlyTheCapIsKept(t *testing.T) {
	cfg := testConfig()
	cfg.MaxImageBytes = 1000
	env, _ := testEnv([]string{"127.0.0.1"}, nil)
	_, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/i" {
			w.Write(make([]byte, 1000))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<head><title>T</title><meta property="og:image" content="/i"></head>`)
	}))
	p, err := newGuarded(cfg, env).Preview(context.Background(), base+"/")
	if err != nil || len(p.Image) != 1000 {
		t.Fatalf("p = %+v err = %v", p, err)
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	cfg := testConfig()
	cfg.PageTimeout = 200 * time.Millisecond
	env, _ := testEnv([]string{"127.0.0.1"}, nil)
	release := make(chan struct{})
	defer close(release)
	_, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // a target that accepts and never answers
		case <-release:
		case <-r.Context().Done():
		}
	}))
	g := newGuarded(cfg, env)
	start := time.Now()
	_, err := g.Preview(context.Background(), base+"/")
	if ClassOf(err) != ClassTimeout {
		t.Fatalf("err = %v, want timeout", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}

	// A hang-up cancels the work.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	cfg.PageTimeout = 5 * time.Second
	start = time.Now()
	if _, err := newGuarded(cfg, env).Preview(ctx, base+"/"); err == nil || time.Since(start) > time.Second {
		t.Fatalf("cancel: err = %v after %s", err, time.Since(start))
	}
}

func TestDNSFailureIsClassified(t *testing.T) {
	env, _ := testEnv(nil, map[string][][]string{})
	_, err := newGuarded(testConfig(), env).Preview(context.Background(), "http://nowhere.test/")
	if ClassOf(err) != ClassDNS {
		t.Fatalf("err = %v, want dns", err)
	}
}

// A name with one refused and one allowed answer connects to the allowed one:
// the attacker adding a private A record changes nothing.
func TestMixedAnswersConnectOnlyToAnAllowedOne(t *testing.T) {
	_, base := serve(t, htmlHandler(headHTML))
	env, _ := testEnv([]string{"127.0.0.1"}, map[string][][]string{
		"mixed.test": {{"10.0.0.1", "127.0.0.1"}},
	})
	p, err := newGuarded(testConfig(), env).Preview(context.Background(), "http://mixed.test:"+portOf(strings.TrimPrefix(base, "http://"))+"/")
	if err != nil || p.Title != "OG title" {
		t.Fatalf("p = %+v err = %v", p, err)
	}
}

func TestNoErrorTextCarriesTheURL(t *testing.T) {
	env, _ := testEnv([]string{"127.0.0.1"}, nil)
	g := newGuarded(testConfig(), env)
	for _, raw := range []string{
		"http://127.0.0.2:9/secret/path?token=abc",
		"http://nowhere.invalid/secret/path?token=abc",
		"ftp://x/secret/path?token=abc",
	} {
		_, err := g.Preview(context.Background(), raw)
		if err == nil {
			t.Fatalf("%s: no error", raw)
		}
		for _, leak := range []string{"secret", "token", "abc", "nowhere"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: error text %q contains %q", raw, err.Error(), leak)
			}
		}
	}
}

func TestClassOfForeignErrors(t *testing.T) {
	if ClassOf(errors.New("x")) != ClassUnavailable {
		t.Fatal("a foreign error is unavailable")
	}
}

// The seam: a second implementation (the estate unfurl service, IDEA-66) is a
// Previewer, and a caller that holds the interface cannot tell which it has.
type fakePreviewer struct{ p *Preview }

func (f fakePreviewer) Preview(context.Context, string) (*Preview, error) { return f.p, nil }

func TestPreviewerIsASeam(t *testing.T) {
	var _ Previewer = fakePreviewer{}
	var _ Previewer = (*Guarded)(nil)
}
