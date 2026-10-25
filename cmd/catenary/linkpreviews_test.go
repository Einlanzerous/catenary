package main

// CANT-322 (CANT-308 row 3): POST /link-previews end to end through setup(), over
// Postgres and a media directory, with a Previewer DOUBLE. Nothing here opens an
// outbound socket: the fetcher is replaced through newPreviewer, so what is under
// test is the endpoint around it (statuses, limiter, cache, ingest, handle), and
// the fetcher's own SSRF defense is internal/linkpreview's tests and CANT-326.

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/linkpreview"
	"github.com/magos/catenary/internal/wire"
)

// fakePreviewer answers from fn and counts how often it was asked.
type fakePreviewer struct {
	calls atomic.Int64
	fn    func(ctx context.Context, rawURL string) (*linkpreview.Preview, error)
}

func (f *fakePreviewer) Preview(ctx context.Context, rawURL string) (*linkpreview.Preview, error) {
	f.calls.Add(1)
	return f.fn(ctx, rawURL)
}

// swapPreviewer installs f as setup()'s fetcher for the test and counts builds,
// so "the switch is off" can assert the constructor was never even reached.
func swapPreviewer(t *testing.T, f linkpreview.Previewer) *atomic.Int64 {
	t.Helper()
	var builds atomic.Int64
	old := newPreviewer
	newPreviewer = func(linkpreview.Config) linkpreview.Previewer {
		builds.Add(1)
		return f
	}
	t.Cleanup(func() { newPreviewer = old })
	return &builds
}

type previewRig struct {
	*uploadRig
	bobToken string
	log      *recorder
}

func newPreviewRig(t *testing.T, enabled bool, tune func(*config.LinkPreviews)) *previewRig {
	t.Helper()
	dir := t.TempDir()
	log := &recorder{}
	ctx, pool, st, d := processFixtureWith(t, slog.New(log), func(c *config.Config) {
		c.MediaDir = dir
		c.MediaURLKeys = "1=" + strings.Repeat("ab", 32)
		c.LinkPreviews = config.DefaultLinkPreviews()
		c.LinkPreviews.Enabled = enabled
		if tune != nil {
			tune(&c.LinkPreviews)
		}
	})
	// The 0014 down refuses while a link card exists and the next test's reset is
	// MigrateDown to empty, so every card made here is removed on the way out.
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM attachments WHERE kind = 'link'`, `DELETE FROM uploads WHERE kind = 'link'`} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatalf("cleanup: %s: %v", q, err)
			}
		}
	})
	ada := mkUser(ctx, t, pool, "ada", "Ada")
	bob := mkUser(ctx, t, pool, "bob", "Bob")
	group := mkGroup(ctx, t, pool, "A", ada, bob)
	token := func(u uuid.UUID, device string) string {
		issued, err := st.IssueEnrollmentToken(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		code, body := do(t, d.router, enrollRequest(t, issued.Plaintext, device))
		if code != http.StatusOK {
			t.Fatalf("enroll = %d: %s", code, body)
		}
		var e wire.EnrollResponse
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatal(err)
		}
		return string(e.AccessToken)
	}
	return &previewRig{
		uploadRig: &uploadRig{t: t, ctx: ctx, pool: pool, d: d, dir: dir, token: token(ada, "laptop"), ada: ada, bob: bob, group: group},
		bobToken:  token(bob, "phone"),
		log:       log,
	}
}

func (r *previewRig) post(token string, body any) *httptest.ResponseRecorder {
	r.t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			r.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(http.MethodPost, "/link-previews", rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.d.router.ServeHTTP(rec, req)
	return rec
}

func (r *previewRig) postURL(token, u string) *httptest.ResponseRecorder {
	return r.post(token, map[string]string{"url": u})
}

func decodeCard(t *testing.T, rec *httptest.ResponseRecorder) wire.LinkPreviewResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var out wire.LinkPreviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body)
	}
	return out
}

// exifJPEG is a JPEG whose APP1 segment carries a marker string, standing in for
// a GPS tag. Ingest re-encodes, so the marker must not survive to the stored blob.
func exifJPEG(t *testing.T, marker string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 40, 30))
	for x := 0; x < 40; x++ {
		for y := 0; y < 30; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 6), uint8(y * 8), 90, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	raw := b.Bytes()
	payload := append([]byte("Exif\x00\x00"), []byte(marker)...)
	seg := append([]byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}, payload...)
	out := append([]byte{}, raw[:2]...)
	out = append(out, seg...)
	return append(out, raw[2:]...)
}

func okPreview(rawURL string, image []byte) *linkpreview.Preview {
	return &linkpreview.Preview{
		URL: rawURL, FinalURL: rawURL,
		Title: "A title", Description: "A description.", SiteName: "Example",
		Image: image,
	}
}

func TestWithTheSwitchOffTheRouteIsA404AndNoFetcherIsBuilt(t *testing.T) {
	f := &fakePreviewer{fn: func(context.Context, string) (*linkpreview.Preview, error) {
		t.Error("the fetcher was asked with the switch off")
		return nil, nil
	}}
	builds := swapPreviewer(t, f)
	r := newPreviewRig(t, false, nil)

	rec := r.postURL(r.token, "https://example.com/a")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /link-previews with the switch off = %d; want the router's 404", rec.Code)
	}
	// The same answer an unknown path gets: the route does not exist.
	other := httptest.NewRecorder()
	r.d.router.ServeHTTP(other, httptest.NewRequest(http.MethodPost, "/no-such-route", nil))
	if rec.Code != other.Code || rec.Body.String() != other.Body.String() {
		t.Errorf("not the router's own 404: %q vs %q", rec.Body, other.Body)
	}
	if builds.Load() != 0 || f.calls.Load() != 0 {
		t.Errorf("builds = %d, calls = %d; want no fetcher at all", builds.Load(), f.calls.Load())
	}
}

func TestAnUnauthenticatedCallIs401BeforeTheURLIsRead(t *testing.T) {
	f := &fakePreviewer{fn: func(context.Context, string) (*linkpreview.Preview, error) {
		t.Error("the fetcher was asked for an unauthenticated caller")
		return nil, nil
	}}
	swapPreviewer(t, f)
	r := newPreviewRig(t, true, nil)

	for name, c := range map[string]struct {
		token string
		body  any
	}{
		"no credential":  {"", map[string]string{"url": "https://example.com/"}},
		"bad credential": {"not-a-token", map[string]string{"url": "https://example.com/"}},
		// The body is garbage, and the answer is still 401, not 400: nothing about
		// the request is looked at before the caller is known.
		"garbage body": {"", "{not json"},
		"bad url":      {"", map[string]string{"url": "ftp://example.com/"}},
	} {
		if rec := r.post(c.token, c.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d; want 401", name, rec.Code)
		}
	}
	if f.calls.Load() != 0 {
		t.Errorf("fetcher calls = %d", f.calls.Load())
	}
}

func TestAMalformedRequestIs400AndOpensNothing(t *testing.T) {
	f := &fakePreviewer{fn: func(context.Context, string) (*linkpreview.Preview, error) {
		t.Error("the fetcher was asked for a malformed request")
		return nil, nil
	}}
	swapPreviewer(t, f)
	r := newPreviewRig(t, true, nil)

	long := "https://example.com/" + strings.Repeat("a", 2100)
	for name, body := range map[string]any{
		"not json":         "{nope",
		"no url":           map[string]string{},
		"empty url":        map[string]string{"url": ""},
		"ftp":              map[string]string{"url": "ftp://example.com/x"},
		"javascript":       map[string]string{"url": "javascript:alert(1)"},
		"no scheme":        map[string]string{"url": "example.com/x"},
		"userinfo":         map[string]string{"url": "https://user:pw@example.com/"},
		"port 8080":        map[string]string{"url": "https://example.com:8080/"},
		"port on a name":   map[string]string{"url": "http://example.com:22/"},
		"over 2048 bytes":  map[string]string{"url": long},
		"no host":          map[string]string{"url": "https:///path"},
		"array not object": "[]",
	} {
		if rec := r.post(r.token, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d (%s); want 400", name, rec.Code, rec.Body)
		}
	}
	// A body past the read bound is a 400, not a hang or a 500.
	if rec := r.post(r.token, `{"url":"`+strings.Repeat("a", 20<<10)+`"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("huge body = %d; want 400", rec.Code)
	}
	if f.calls.Load() != 0 {
		t.Errorf("fetcher calls = %d; a 400 opens nothing", f.calls.Load())
	}
}

func TestACardIsStoredThroughIngestAndItsHandleSendsOnce(t *testing.T) {
	const marker = "GPS-LAT-48.8584-LON-2.2945"
	raw := exifJPEG(t, marker)
	if !bytes.Contains(raw, []byte(marker)) {
		t.Fatal("the fixture does not carry the marker")
	}
	f := &fakePreviewer{fn: func(_ context.Context, u string) (*linkpreview.Preview, error) {
		return okPreview(u, raw), nil
	}}
	swapPreviewer(t, f)
	r := newPreviewRig(t, true, nil)

	rec := r.postURL(r.token, "HTTPS://Example.com/watch?v=1#frag")
	got := decodeCard(t, rec)
	if got.Preview.URL != "https://example.com/watch?v=1" {
		t.Errorf("url = %q; want the canonical form", got.Preview.URL)
	}
	if got.Preview.Title == nil || *got.Preview.Title != "A title" ||
		got.Preview.Description == nil || got.Preview.SiteName == nil || *got.Preview.SiteName != "Example" {
		t.Errorf("text fields = %+v", got.Preview)
	}
	p := got.Preview
	if p.ImageURL == nil || p.ImageWidth == nil || p.ImageHeight == nil || p.ImagePlaceholder == nil ||
		*p.ImageWidth != 40 || *p.ImageHeight != 30 || *p.ImagePlaceholder == "" {
		t.Fatalf("image fields = %+v", p)
	}

	// Served by the media route, EXIF stripped, thumbnail present.
	full := r.get(*p.ImageURL, nil)
	if full.Code != http.StatusOK || full.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("GET image_url = %d %s", full.Code, full.Header().Get("Content-Type"))
	}
	if bytes.Contains(full.Body.Bytes(), []byte(marker)) || bytes.Contains(full.Body.Bytes(), []byte("Exif")) {
		t.Error("the stored card image still carries EXIF")
	}
	thumb := r.get(*p.ImageURL+"&v=thumb", nil)
	if thumb.Code != http.StatusOK || thumb.Body.Len() == 0 || bytes.Equal(thumb.Body.Bytes(), full.Body.Bytes()) {
		t.Errorf("GET ?v=thumb = %d, %d bytes (same as full: %v)", thumb.Code, thumb.Body.Len(), bytes.Equal(thumb.Body.Bytes(), full.Body.Bytes()))
	}
	if bytes.Contains(thumb.Body.Bytes(), []byte(marker)) {
		t.Error("the thumbnail carries EXIF")
	}

	// The handle is uploader-bound: Bob cannot send Ada's card.
	handle := uuid.MustParse(got.UploadID)
	if code, _ := r.send(r.bobToken, att("link", handle)); code != http.StatusNotFound {
		t.Errorf("another account sending the handle = %d; want 404 upload_not_found", code)
	}
	if r.consumed(handle) {
		t.Fatal("a refused send consumed the handle")
	}
	// Ada's send takes it, once.
	code, body := r.send(r.token, att("link", handle))
	if code != http.StatusOK {
		t.Fatalf("send = %d: %s", code, body)
	}
	if code, _ := r.send(r.token, att("link", handle)); code != http.StatusNotFound {
		t.Errorf("a second send of the same handle = %d; want 404 upload_not_found", code)
	}
	var sent struct {
		Attachments []struct {
			Kind     string `json:"kind"`
			URL      string `json:"url"`
			ImageURL string `json:"image_url"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(body, &sent); err != nil || len(sent.Attachments) != 1 ||
		sent.Attachments[0].Kind != "link" || sent.Attachments[0].URL != got.Preview.URL ||
		sent.Attachments[0].ImageURL != *p.ImageURL {
		t.Errorf("sent attachment = %s (%v)", body, err)
	}
}

func TestEveryFetchFailureIsOneIdenticalEmpty204(t *testing.T) {
	classes := []linkpreview.Class{
		linkpreview.ClassBlocked, linkpreview.ClassTimeout, linkpreview.ClassTooLarge, linkpreview.ClassNotHTML,
		linkpreview.ClassEmpty, linkpreview.ClassDNS, linkpreview.ClassUnavailable,
	}
	byHost := map[string]error{}
	for _, c := range classes {
		byHost[strings.ReplaceAll(string(c), "_", "-")+".example"] = linkpreview.NewError(c, "loopback")
	}
	byHost["plain.example"] = context.DeadlineExceeded // an error that is not ours at all
	f := &fakePreviewer{fn: func(_ context.Context, u string) (*linkpreview.Preview, error) {
		for h, err := range byHost {
			if strings.Contains(u, "//"+h+"/") {
				return nil, err
			}
		}
		t.Errorf("unexpected fetch of %q", u)
		return nil, nil
	}}
	swapPreviewer(t, f)
	r := newPreviewRig(t, true, func(l *config.LinkPreviews) { l.Burst, l.RatePerMinute = 100, 100 })

	var want *httptest.ResponseRecorder
	for host := range byHost {
		rec := r.postURL(r.token, "https://"+host+"/secret/path?token=hunter2")
		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("%s = %d %q; want an empty 204", host, rec.Code, rec.Body)
		}
		if want == nil {
			want = rec
			continue
		}
		if len(rec.Header()) != len(want.Header()) {
			t.Errorf("%s: headers %v differ from %v", host, rec.Header(), want.Header())
		}
		for k, v := range want.Header() {
			if strings.Join(rec.Header()[k], ",") != strings.Join(v, ",") {
				t.Errorf("%s: header %s = %v; others %v", host, k, rec.Header()[k], v)
			}
		}
	}

	// The class reaches the log, the URL's path and query never do.
	for _, rec := range r.log.find("link preview") {
		rec.Attrs(func(a slog.Attr) bool {
			if s := a.Value.String(); strings.Contains(s, "hunter2") || strings.Contains(s, "secret") {
				t.Errorf("the log line carries the URL: %s=%s", a.Key, s)
			}
			return true
		})
	}
	seen := map[string]bool{}
	for _, rec := range r.log.find("link preview") {
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "result" {
				seen[a.Value.String()] = true
			}
			return true
		})
	}
	for _, c := range classes {
		if !seen[string(c)] {
			t.Errorf("no log line with result=%s (saw %v)", c, seen)
		}
	}
	if blocked := r.log.find("link preview blocked"); len(blocked) != 1 {
		t.Errorf("blocked warn lines = %d; want 1", len(blocked))
	}
	for _, rec := range r.log.find("link preview blocked") {
		if rec.Level != slog.LevelWarn {
			t.Errorf("blocked logged at %v; want warn", rec.Level)
		}
	}
}

func TestAnImageWeCannotUseDegradesToATextCardOrNothing(t *testing.T) {
	f := &fakePreviewer{fn: func(_ context.Context, u string) (*linkpreview.Preview, error) {
		p := okPreview(u, []byte("<html>this is not an image</html>"))
		if strings.Contains(u, "//untitled.example/") {
			p.Title = ""
		}
		if strings.Contains(u, "//textonly.example/") {
			p.Image = nil
		}
		return p, nil
	}}
	swapPreviewer(t, f)
	r := newPreviewRig(t, true, nil)

	for _, host := range []string{"badimage.example", "textonly.example"} {
		got := decodeCard(t, r.postURL(r.token, "https://"+host+"/"))
		if got.Preview.ImageURL != nil || got.Preview.ImageWidth != nil || got.Preview.ImageHeight != nil || got.Preview.ImagePlaceholder != nil {
			t.Errorf("%s: a text-only card carries image fields: %+v", host, got.Preview)
		}
		// And the text-only handle sends.
		if code, body := r.send(r.token, att("link", uuid.MustParse(got.UploadID))); code != http.StatusOK {
			t.Errorf("%s: send = %d: %s", host, code, body)
		}
	}
	if rec := r.postURL(r.token, "https://untitled.example/"); rec.Code != http.StatusNoContent {
		t.Errorf("no title and an unusable image = %d; want 204", rec.Code)
	}
}

func TestTheAccountLimiterIs429WithRetryAfter(t *testing.T) {
	f := &fakePreviewer{fn: func(_ context.Context, u string) (*linkpreview.Preview, error) {
		return okPreview(u, nil), nil
	}}
	swapPreviewer(t, f)
	r := newPreviewRig(t, true, func(l *config.LinkPreviews) { l.Burst, l.RatePerMinute = 2, 6 })

	for i := 0; i < 2; i++ {
		if rec := r.postURL(r.token, "https://example.com/"+strings.Repeat("a", i+1)); rec.Code != http.StatusOK {
			t.Fatalf("call %d = %d; want 200 inside the burst", i, rec.Code)
		}
	}
	rec := r.postURL(r.token, "https://example.com/over")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over the burst = %d; want 429", rec.Code)
	}
	var se wire.ServerError
	if err := json.Unmarshal(rec.Body.Bytes(), &se); err != nil {
		t.Fatalf("429 body: %v: %s", err, rec.Body)
	}
	if se.Code != wire.ErrorCodeRateLimited || !se.Retryable || se.RetryAfterSec == nil || *se.RetryAfterSec < 1 {
		t.Errorf("429 body = %+v; want rate_limited, retryable, retry_after_sec >= 1", se)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After header")
	}
	if f.calls.Load() != 2 {
		t.Errorf("fetcher calls = %d; the limited call must not fetch", f.calls.Load())
	}
	// The bucket is per account: Bob is untouched by Ada's.
	if rec := r.postURL(r.bobToken, "https://example.com/bobs"); rec.Code != http.StatusOK {
		t.Errorf("another account = %d; want 200", rec.Code)
	}
}

func TestTheServerWideCapIs429AndAFreedSlotWorksAgain(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := &fakePreviewer{fn: func(_ context.Context, u string) (*linkpreview.Preview, error) {
		if strings.Contains(u, "//slow.example/") {
			entered <- struct{}{}
			<-release
		}
		return okPreview(u, nil), nil
	}}
	swapPreviewer(t, f)
	r := newPreviewRig(t, true, func(l *config.LinkPreviews) {
		l.Concurrency, l.Burst, l.RatePerMinute = 1, 50, 50
	})

	var wg sync.WaitGroup
	var first *httptest.ResponseRecorder
	wg.Add(1)
	go func() { defer wg.Done(); first = r.postURL(r.token, "https://slow.example/") }()
	<-entered

	rec := r.postURL(r.bobToken, "https://other.example/")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second fetch with the one slot taken = %d; want 429", rec.Code)
	}
	var se wire.ServerError
	if err := json.Unmarshal(rec.Body.Bytes(), &se); err != nil || se.Code != wire.ErrorCodeRateLimited || se.RetryAfterSec == nil || *se.RetryAfterSec < 1 {
		t.Errorf("429 body = %s (%v)", rec.Body, err)
	}
	close(release)
	wg.Wait()
	if first.Code != http.StatusOK {
		t.Errorf("the in-flight fetch = %d", first.Code)
	}
	if rec := r.postURL(r.bobToken, "https://other.example/"); rec.Code != http.StatusOK {
		t.Errorf("after the slot freed = %d; want 200", rec.Code)
	}
}

func TestACacheHitOpensNoSocketButMintsAFreshHandle(t *testing.T) {
	raw := exifJPEG(t, "x")
	f := &fakePreviewer{fn: func(_ context.Context, u string) (*linkpreview.Preview, error) {
		if strings.Contains(u, "//down.example/") {
			return nil, linkpreview.NewError(linkpreview.ClassTimeout, "")
		}
		return okPreview(u, raw), nil
	}}
	swapPreviewer(t, f)
	r := newPreviewRig(t, true, nil)

	a := decodeCard(t, r.postURL(r.token, "https://example.com/post?x=1"))
	// The same link, spelled differently, from another account.
	b := decodeCard(t, r.postURL(r.bobToken, "HTTPS://EXAMPLE.com/post?x=1#section"))
	c := decodeCard(t, r.postURL(r.token, "https://example.com/post?x=1"))
	if f.calls.Load() != 1 {
		t.Fatalf("fetcher calls = %d; the cache must answer the second and third", f.calls.Load())
	}
	if a.UploadID == b.UploadID || a.UploadID == c.UploadID || b.UploadID == c.UploadID {
		t.Fatalf("handles repeat: %s %s %s", a.UploadID, b.UploadID, c.UploadID)
	}
	if *a.Preview.ImageURL != *b.Preview.ImageURL || a.Preview.URL != b.Preview.URL {
		t.Errorf("a hit serves a different card: %+v vs %+v", a.Preview, b.Preview)
	}
	// Each handle belongs to whoever asked, and Bob's sends as Bob.
	if code, body := r.send(r.bobToken, att("link", uuid.MustParse(b.UploadID))); code != http.StatusOK {
		t.Errorf("bob sending his minted handle = %d: %s", code, body)
	}
	if code, _ := r.send(r.bobToken, att("link", uuid.MustParse(a.UploadID))); code != http.StatusNotFound {
		t.Errorf("bob sending ada's handle = %d; want 404", code)
	}

	// A failure is remembered too, so a dead link is not re-fetched per member.
	for i := 0; i < 3; i++ {
		if rec := r.postURL(r.bobToken, "https://down.example/"); rec.Code != http.StatusNoContent {
			t.Fatalf("down link = %d", rec.Code)
		}
	}
	if f.calls.Load() != 2 {
		t.Errorf("fetcher calls = %d; want 2 (one card, one failure)", f.calls.Load())
	}
}

func TestACachedCardMeansTheLimiterStillCounts(t *testing.T) {
	f := &fakePreviewer{fn: func(_ context.Context, u string) (*linkpreview.Preview, error) {
		return okPreview(u, nil), nil
	}}
	swapPreviewer(t, f)
	r := newPreviewRig(t, true, func(l *config.LinkPreviews) { l.Burst, l.RatePerMinute = 2, 6 })
	r.postURL(r.token, "https://example.com/same")
	r.postURL(r.token, "https://example.com/same")
	if rec := r.postURL(r.token, "https://example.com/same"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a hit that mints a handle is not free of the limiter: %d", rec.Code)
	}
}
