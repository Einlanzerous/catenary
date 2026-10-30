//go:build linkpreviewtest

package main

// CANT-326 (CANT-308 row 6) — the end-to-end proof of link previews.
//
// Everything here is the real thing except the network: a real HTTP server over
// the real router, Postgres, a media directory and signed media URLs, the real
// service (limiter, cache, ingest, handle) and the real guarded fetcher with its
// production dialer and judge. The internet is replaced by ONE fake origin bound
// to 127.0.0.77 (a non-public address the production judge refuses), which
// serves every fake hostname by Host header, and by linkpreview.NewForTest, the
// allowance that exists only under the linkpreviewtest build tag (see
// internal/linkpreview/testnet.go and its guard test). Every other address,
// 10.0.0.5, 169.254.169.254, 127.0.0.1, is still judged by the production table.
//
// Run by verify.sh's CANT-326 step: go test -tags linkpreviewtest.

import (
	"bytes"

	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/linkpreview"
	"github.com/magos/catenary/internal/wire"
)

const originIP = "127.0.0.77"

// originPNG is a 48x32 picture the fake origin serves as og:image.
func originPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 48, 32))
	for x := 0; x < 48; x++ {
		for y := 0; y < 32; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 5), uint8(y * 7), 120, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type fakeOrigin struct {
	port int
	mu   sync.Mutex
	hits map[string]int // "host/path" -> requests
}

func (o *fakeOrigin) hit(k string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hits[k]
}

func page(title, image string) string {
	img := ""
	if image != "" {
		img = `<meta property="og:image" content="` + image + `">`
	}
	return `<!doctype html><html><head><meta charset="utf-8"><title>` + title + `</title>` +
		`<meta property="og:title" content="` + title + `"><meta property="og:description" content="Fake origin.">` + img +
		`</head><body>hi</body></html>`
}

// newFakeOrigin serves every fake hostname from one socket on a non-public
// address, routed by Host and path.
func newFakeOrigin(t *testing.T, pic []byte) *fakeOrigin {
	t.Helper()
	ln, err := net.Listen("tcp", originIP+":0")
	if err != nil {
		t.Skipf("cannot bind %s: %v", originIP, err)
	}
	o := &fakeOrigin{port: ln.Addr().(*net.TCPAddr).Port, hits: map[string]int{}}
	big := "<!doctype html><html><head><meta charset=\"utf-8\">" + strings.Repeat("<!-- padding padding padding padding -->", 8000) +
		"<title>Too late</title></head></html>"
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		o.mu.Lock()
		o.hits[host+r.URL.Path]++
		o.mu.Unlock()
		html := func(body string) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, body)
		}
		switch host + r.URL.Path {
		case "short.test/s": // a shortener: leaves the pasted site
			http.Redirect(w, r, "http://www.target-site.test/watch", http.StatusFound)
		case "www.target-site.test/watch":
			html(page("The target's own title", "/thumb.png"))
		case "www.target-site.test/thumb.png", "fake.test/thumb.png", "rebind.test/thumb.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(pic)
		case "fake.test/same-site": // stays on the pasted site
			http.Redirect(w, r, "http://www.fake.test/video", http.StatusMovedPermanently)
		case "www.fake.test/video", "fake.test/video":
			html(page("A video on the fake site", "/thumb.png"))
		case "fake.test/to-private":
			http.Redirect(w, r, "http://10.0.0.5/admin", http.StatusFound)
		case "fake.test/to-metadata":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "fake.test/to-loopback":
			http.Redirect(w, r, "http://127.0.0.1/", http.StatusFound)
		case "fake.test/to-localhost":
			http.Redirect(w, r, "http://localhost/", http.StatusFound)
		case "fake.test/to-private-name":
			http.Redirect(w, r, "http://private.test/", http.StatusFound)
		case "rebind.test/page": // its second resolution is private
			html(page("A page whose picture host rebinds", "http://rebind.test/thumb.png"))
		case "big.test/page":
			html(big)
		case "slow.test/page":
			select {
			case <-time.After(5 * time.Second):
				html(page("Too slow", ""))
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return o
}

type e2eRig struct {
	*rig
	dir     string
	origin  *fakeOrigin
	lookups *linkpreview.TestLookups
	ada     uuid.UUID
	bob     uuid.UUID
	conv    uuid.UUID
	adaTok  string
}

func newE2ERig(t *testing.T) *e2eRig {
	t.Helper()
	pic := originPNG(t)
	origin := newFakeOrigin(t, pic)
	e := &e2eRig{origin: origin, dir: t.TempDir()}

	old := newPreviewer
	newPreviewer = func(c linkpreview.Config) linkpreview.Previewer {
		g, lk := linkpreview.NewForTest(c, linkpreview.TestNet{
			Public: []string{originIP},
			Hosts: map[string][][]string{
				"short.test":           {{originIP}},
				"www.target-site.test": {{originIP}},
				"fake.test":            {{originIP}},
				"www.fake.test":        {{originIP}},
				"big.test":             {{originIP}},
				"slow.test":            {{originIP}},
				"private.test":         {{"10.0.0.5"}},
				"rebind.test":          {{originIP}, {"10.0.0.5"}}, // public once, then private
				"localhost":            {{"127.0.0.1"}},
			},
			Ports: map[int]int{80: origin.port},
		})
		e.lookups = lk
		return g
	}
	t.Cleanup(func() { newPreviewer = old })

	log := &recorder{}
	ctx, pool, st, d := processFixtureWith(t, slog.New(log), func(c *config.Config) {
		c.MediaDir = e.dir
		c.MediaURLKeys = "1=" + strings.Repeat("ab", 32)
		c.LinkPreviews = config.DefaultLinkPreviews()
		c.LinkPreviews.Enabled = true
		c.LinkPreviews.Burst, c.LinkPreviews.RatePerMinute = 100, 100
		c.LinkPreviews.MaxPageBytes = 64 << 10
		c.LinkPreviews.PageTimeout = 400 * time.Millisecond
		c.LinkPreviews.ImageTimeout = 400 * time.Millisecond
		c.LinkPreviews.TotalTimeout = 2 * time.Second
	})
	srv := httptest.NewServer(d.router)
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM attachments WHERE kind = 'link'`, `DELETE FROM uploads WHERE kind = 'link'`} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatalf("cleanup: %s: %v", q, err)
			}
		}
	})
	e.rig = &rig{t: t, ctx: ctx, pool: pool, st: st, d: d, log: log, base: srv.URL, since: dbNow(ctx, t, pool)}
	e.runListener()
	e.ada = mkUser(ctx, t, pool, "ada", "Ada")
	e.bob = mkUser(ctx, t, pool, "bob", "Bob")
	e.conv = mkGroup(ctx, t, pool, "A", e.ada, e.bob)
	e.adaTok = string(e.enroll(e.ada, "laptop").AccessToken)
	return e
}

// call is an HTTP request to the real listener.
func (e *e2eRig) call(method, path, token string, body any) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(e.ctx, method, e.base+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (e *e2eRig) preview(token, u string) (*http.Response, []byte) {
	e.t.Helper()
	return e.call(http.MethodPost, "/link-previews", token, map[string]string{"url": u})
}

func (e *e2eRig) card(token, u string) wire.LinkPreviewResponse {
	e.t.Helper()
	resp, b := e.preview(token, u)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("POST /link-previews %s = %d: %s", u, resp.StatusCode, b)
	}
	var c wire.LinkPreviewResponse
	if err := json.Unmarshal(b, &c); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *e2eRig) uploads() (rows int, files int) {
	e.t.Helper()
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM uploads`).Scan(&rows); err != nil {
		e.t.Fatal(err)
	}
	_ = fs.WalkDir(os.DirFS(e.dir), ".", func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files++
		}
		return nil
	})
	return rows, files
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// compose -> POST /link-previews -> send -> a second device sees the same card.
func TestAComposedLinkCardReachesASecondDeviceUnchanged(t *testing.T) {
	e := newE2ERig(t)
	bobWS := e.open(e.bob, "phone") // bob's first device, already connected
	adaWS := e.open(e.ada, "desk")  // ada's second device, listening too

	// Compose: a shortener that leaves the pasted site.
	pasted := "http://short.test/s"
	got := e.card(e.adaTok, pasted)
	p := got.Preview
	if p.URL != pasted || p.Title == nil || *p.Title != "The target's own title" {
		t.Fatalf("preview = %+v; want the pasted URL and the target's title", p)
	}
	if p.FinalHost == nil || *p.FinalHost != "www.target-site.test" {
		t.Fatalf("final_host = %v; want www.target-site.test (the redirect left the pasted site)", p.FinalHost)
	}
	if p.ImageURL == nil || !strings.HasPrefix(*p.ImageURL, "/media/") || p.ImageWidth == nil || *p.ImageWidth != 48 {
		t.Fatalf("image = %v %v; want a signed /media URL for the 48-wide picture", p.ImageURL, p.ImageWidth)
	}
	if e.lookups.Count("short.test") == 0 || e.origin.hit("short.test/s") != 1 || e.origin.hit("www.target-site.test/watch") != 1 {
		t.Errorf("the fetch did not go through the fake origin: hits %v", e.origin.hits)
	}

	// Send, with the handle, from ada's first device.
	resp, raw := e.call(http.MethodPost, "/conversations/"+e.conv.String()+"/messages", e.adaTok, map[string]any{
		"client_id": uuid.NewString(), "text": "look " + pasted,
		"attachments": []map[string]string{{"kind": "link", "upload_id": got.UploadID}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send = %d: %s", resp.StatusCode, raw)
	}
	var sent wire.Message
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatal(err)
	}
	want := marshal(t, linkOf(t, sent))
	if want != marshal(t, p) {
		t.Errorf("the sent card differs from the previewed one:\n sent    %s\n preview %s", want, marshal(t, p))
	}

	// Second device 1: bob's open socket gets the identical card.
	if g := marshal(t, linkOf(t, bobWS.message())); g != want {
		t.Errorf("bob's socket card differs:\n got  %s\n want %s", g, want)
	}
	// Second device 2: ada's other device, over its socket.
	if g := marshal(t, linkOf(t, adaWS.message())); g != want {
		t.Errorf("ada's second device socket card differs:\n got  %s\n want %s", g, want)
	}
	// And a device that was offline: /sync, for the recipient and for ada's tablet.
	for _, who := range []struct {
		name string
		user uuid.UUID
	}{{"bob", e.bob}, {"ada", e.ada}} {
		tok := string(e.enroll(who.user, who.name+"-late").AccessToken)
		resp, raw := e.call(http.MethodGet, "/sync?after=0", tok, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s /sync = %d: %s", who.name, resp.StatusCode, raw)
		}
		var page wire.SyncResponse
		if err := json.Unmarshal(raw, &page); err != nil || len(page.Messages) != 1 {
			t.Fatalf("%s /sync = %v, %d messages", who.name, err, len(page.Messages))
		}
		if g := marshal(t, linkOf(t, page.Messages[0])); g != want {
			t.Errorf("%s /sync card differs:\n got  %s\n want %s", who.name, g, want)
		}
	}

	// The image is served through the signed URL the card carries.
	resp, img := e.call(http.MethodGet, *p.ImageURL, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET image_url = %d", resp.StatusCode)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(img))
	if err != nil || format != "png" || int64(cfg.Width) != *p.ImageWidth || int64(cfg.Height) != *p.ImageHeight {
		t.Errorf("served image = %v %s %dx%d; want the card's %dx%d", err, format, cfg.Width, cfg.Height, *p.ImageWidth, *p.ImageHeight)
	}
	if resp, _ := e.call(http.MethodGet, strings.Replace(*p.ImageURL, "sig=", "sig=00", 1), "", nil); resp.StatusCode == http.StatusOK {
		t.Error("a tampered signature served the image")
	}

	// A second member pasting the same link is a cache hit: no new fetch, a fresh handle.
	again := e.card(string(e.enroll(e.bob, "phone2").AccessToken), pasted)
	if again.UploadID == got.UploadID || marshal(t, again.Preview) != marshal(t, p) {
		t.Errorf("cache hit: handle %s vs %s, card equal=%v", again.UploadID, got.UploadID, marshal(t, again.Preview) == marshal(t, p))
	}
	if n := e.origin.hit("short.test/s"); n != 1 {
		t.Errorf("origin saw %d fetches of the shortener; want 1 (the second paste is the cache's)", n)
	}
}

// A redirect that stays on the pasted site carries no final_host.
func TestARedirectWithinTheSiteCarriesNoFinalHost(t *testing.T) {
	e := newE2ERig(t)
	c := e.card(e.adaTok, "http://fake.test/same-site")
	if c.Preview.FinalHost != nil {
		t.Errorf("final_host = %q on a same-site redirect; want absent", *c.Preview.FinalHost)
	}
	if c.Preview.Title == nil || *c.Preview.Title != "A video on the fake site" {
		t.Errorf("title = %v", c.Preview.Title)
	}
}

// Every refusal, through the real HTTP stack, is the same empty 204, and none
// leaves a row or a file behind.
func TestRefusalsThroughTheRealStackAreOneUniform204AndStoreNothing(t *testing.T) {
	e := newE2ERig(t)
	cases := []struct{ name, url string }{
		{"private redirect (10/8)", "http://fake.test/to-private"},
		{"redirect to the metadata address", "http://fake.test/to-metadata"},
		{"redirect to 127.0.0.1", "http://fake.test/to-loopback"},
		{"redirect to localhost", "http://fake.test/to-localhost"},
		{"redirect to a name that resolves privately", "http://fake.test/to-private-name"},
		{"a name that resolves privately", "http://private.test/page"},
		{"an address literal", "http://10.0.0.5/page"},
		{"an oversize page", "http://big.test/page"},
		{"a slow page", "http://slow.test/page"},
		{"an unknown host", "http://nonexistent.test/page"},
	}
	var first *http.Response
	for _, c := range cases {
		start := time.Now()
		resp, body := e.preview(e.adaTok, c.url)
		if resp.StatusCode != http.StatusNoContent || len(body) != 0 {
			t.Errorf("%s: %d %q; want an empty 204", c.name, resp.StatusCode, body)
			continue
		}
		if time.Since(start) > 3*time.Second {
			t.Errorf("%s: took %v", c.name, time.Since(start))
		}
		if first == nil {
			first = resp
			continue
		}
		for _, h := range []string{"Content-Type", "Content-Length", "Cache-Control"} {
			if resp.Header.Get(h) != first.Header.Get(h) {
				t.Errorf("%s: header %s = %q; the first refusal had %q", c.name, h, resp.Header.Get(h), first.Header.Get(h))
			}
		}
	}
	if rows, files := e.uploads(); rows != 0 || files != 0 {
		t.Errorf("after the refusals: %d uploads rows and %d media files; want none", rows, files)
	}

	// The reasons reached the log, by class, and the URL did not.
	classes := map[string]int{}
	for _, rec := range e.log.find("link preview") {
		var res, detail string
		rec.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "result":
				res = a.Value.String()
			case "detail":
				detail = a.Value.String()
			}
			if s := a.Value.String(); strings.Contains(s, "10.0.0.5") || strings.Contains(s, "169.254") {
				t.Errorf("the log carries an address from the URL: %s=%s", a.Key, s)
			}
			return true
		})
		classes[res]++
		_ = detail
	}
	for _, want := range []string{"blocked", "too_large", "timeout", "dns"} {
		if classes[want] == 0 {
			t.Errorf("no log line with result=%s (saw %v)", want, classes)
		}
	}
	if n := len(e.log.find("link preview blocked")); n < 6 {
		t.Errorf("blocked warn lines = %d; want one per blocked case (6)", n)
	}
	// The private targets were refused before they were connected to: the fake
	// origin only ever saw the pasted hosts.
	for k := range e.origin.hits {
		if strings.HasPrefix(k, "10.0.0.5") || strings.HasPrefix(k, "169.254.169.254") || strings.HasPrefix(k, "127.0.0.1") || strings.HasPrefix(k, "localhost") {
			t.Errorf("the origin served %s: a refused address was connected to", k)
		}
	}
}

// DNS rebinding: a name that is public when the page is fetched and private when
// the picture is. The picture is refused at connect time; the card survives as
// text.
func TestARebindingHostnameLosesItsImageAtConnectTime(t *testing.T) {
	e := newE2ERig(t)
	c := e.card(e.adaTok, "http://rebind.test/page")
	p := c.Preview
	if p.Title == nil || *p.Title != "A page whose picture host rebinds" {
		t.Fatalf("title = %v", p.Title)
	}
	if p.ImageURL != nil || p.ImageWidth != nil {
		t.Errorf("image = %v; the rebound host's picture must not be fetched", p.ImageURL)
	}
	if n := e.lookups.Count("rebind.test"); n != 2 {
		t.Errorf("rebind.test resolved %d times; want 2 (the page, then the picture)", n)
	}
	if n := e.origin.hit("rebind.test/thumb.png"); n != 0 {
		t.Errorf("the origin served the rebound picture %d times", n)
	}
	if _, files := e.uploads(); files != 0 {
		t.Errorf("%d media files stored for a text-only card", files)
	}
	var sawBlockedImage bool
	for _, rec := range e.log.find("link preview") {
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "image_class" && a.Value.String() == "blocked" {
				sawBlockedImage = true
			}
			return true
		})
	}
	if !sawBlockedImage {
		t.Error("the log does not record the image as blocked")
	}
}
