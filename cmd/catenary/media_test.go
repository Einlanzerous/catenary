package main

// CANT-311's Done-when, end to end through setup(): the URLs the three
// transports derive are the same bytes, a signed URL serves the blob with the
// headers CANT-47 chose, and every way of getting the signature wrong is refused.

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/media"
)

type servedURLs struct {
	Attachments []struct {
		URL string `json:"url"`
	} `json:"attachments"`
}

func pngBytes(t *testing.T, shade uint8) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.Gray{shade})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// putBlob stores bytes straight into the rig's media directory and returns the key.
func (r *uploadRig) putBlob(b []byte) string {
	r.t.Helper()
	ms, err := media.NewLocal(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	key, _, err := ms.Put(context.Background(), bytes.NewReader(b), 1<<20)
	if err != nil {
		r.t.Fatal(err)
	}
	return key
}

func (r *uploadRig) get(rawURL string, hdr map[string]string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.d.router.ServeHTTP(rec, req)
	return rec
}

// sendImage seeds a ready image upload backed by real blobs and sends it,
// returning the URL the REST response carried.
func (r *uploadRig) sendImage(main, thumb []byte) (restURL string, mainKey string) {
	r.t.Helper()
	mainKey = r.putBlob(main)
	var thumbKey *string
	if thumb != nil {
		k := r.putBlob(thumb)
		thumbKey = &k
	}
	id := uuid.New()
	if _, err := r.pool.Exec(r.ctx, `
		INSERT INTO uploads (id, uploader, kind, storage_key, thumb_key, bytes, width, height, placeholder, filename)
		VALUES ($1, $2, 'image', $3, $4, $5, 4, 4, 'L', 'x.png')`,
		id, r.ada, mainKey, thumbKey, len(main)); err != nil {
		r.t.Fatal(err)
	}
	code, raw := r.send(r.token, att("image", id))
	if code != http.StatusOK {
		r.t.Fatalf("send = %d: %s", code, raw)
	}
	var m servedURLs
	if err := json.Unmarshal(raw, &m); err != nil || len(m.Attachments) != 1 {
		r.t.Fatalf("decode: %v (%s)", err, raw)
	}
	return m.Attachments[0].URL, mainKey
}

func TestASignedMediaURLServesTheBlobAndEveryTamperingIsRefused(t *testing.T) {
	r := newUploadRig(t)
	main, thumb := pngBytes(t, 200), pngBytes(t, 90)
	u, key := r.sendImage(main, thumb)

	// The same URL comes back on /sync: one deriver for every transport.
	var onSync string
	for _, c := range r.syncURLs() {
		onSync = c
	}
	if onSync != u {
		t.Fatalf("REST url %q != /sync url %q", u, onSync)
	}
	if !strings.HasPrefix(u, "/media/"+key+"?kid=1&sig=") {
		t.Fatalf("url = %q", u)
	}

	rec := r.get(u, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), main) {
		t.Fatalf("GET %s = %d", u, rec.Code)
	}
	h := rec.Header()
	if h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Cache-Control") != "private, max-age=31536000, immutable" || h.Get("Content-Disposition") != "" {
		t.Errorf("headers = %v", h)
	}

	// Range.
	rec = r.get(u, map[string]string{"Range": "bytes=2-5"})
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), main[2:6]) {
		t.Errorf("range = %d %q", rec.Code, rec.Body.Bytes())
	}

	// The thumbnail is the same URL plus v=thumb, and a different blob.
	rec = r.get(u+"&v=thumb", nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), thumb) {
		t.Errorf("thumb = %d", rec.Code)
	}

	pu, _ := url.Parse(u)
	q := pu.Query()
	signer, err := media.ParseKeys("1=" + strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		target string
		want   int
	}{
		"tampered sig":    {"/media/" + key + "?kid=1&sig=" + q.Get("sig")[1:] + "A", http.StatusForbidden},
		"no sig":          {"/media/" + key + "?kid=1", http.StatusForbidden},
		"unknown kid":     {"/media/" + key + "?kid=7&sig=" + q.Get("sig"), http.StatusForbidden},
		"non-numeric":     {"/media/" + key + "?kid=x&sig=" + q.Get("sig"), http.StatusForbidden},
		"sig of other":    {"/media/" + strings.Repeat("0", 64) + "?kid=1&sig=" + q.Get("sig"), http.StatusForbidden},
		"bad v":           {u + "&v=full", http.StatusBadRequest},
		"signed, no blob": {signer.URL(strings.Repeat("0", 64)), http.StatusNotFound},
	} {
		if rec := r.get(c.target, nil); rec.Code != c.want {
			t.Errorf("%s = %d; want %d", name, rec.Code, c.want)
		}
	}
}

func TestAThumbnailRequestNeverFallsBackToTheFullImage(t *testing.T) {
	r := newUploadRig(t)
	u, _ := r.sendImage(pngBytes(t, 33), nil)
	if rec := r.get(u+"&v=thumb", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("v=thumb with no thumbnail = %d; want 404", rec.Code)
	}
	if rec := r.get(u, nil); rec.Code != http.StatusOK {
		t.Fatalf("the full image = %d", rec.Code)
	}
}

func TestOnlyImagesAndAudioAreServedInline(t *testing.T) {
	r := newUploadRig(t)
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	u, _ := r.sendImage(svg, nil)
	rec := r.get(u, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/octet-stream" || rec.Header().Get("Content-Disposition") != "attachment" {
		t.Errorf("an SVG was served as %q / %q", rec.Header().Get("Content-Type"), rec.Header().Get("Content-Disposition"))
	}
}

func TestARotatedInKeyKeepsEarlierURLsWorking(t *testing.T) {
	r := newUploadRig(t)
	u, key := r.sendImage(pngBytes(t, 7), nil)

	// A second process with kid 2 added: the newest key signs, the old URL verifies.
	rotated := "1=" + strings.Repeat("ab", 32) + ",2=" + strings.Repeat("cd", 32)
	_, _, _, d2 := processFixtureWith(t, slog.New(slog.DiscardHandler), func(c *config.Config) {
		c.MediaDir = r.dir
		c.MediaURLKeys = rotated
	})
	req := httptest.NewRequest(http.MethodGet, u, nil)
	rec := httptest.NewRecorder()
	d2.router.ServeHTTP(rec, req)
	// processFixtureWith reset the database, so the row is gone, but a valid
	// signature on an old kid must get past the 403.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("a kid 1 URL was refused after kid 2 was added")
	}
	_ = key
	s2, err := media.ParseKeys(rotated)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s2.URL(key), "kid=2") {
		t.Fatalf("the rotated signer did not sign with kid 2: %s", s2.URL(key))
	}
}

func TestBootRefusesAMediaDirWithoutGoodKeys(t *testing.T) {
	for name, keys := range map[string]string{"missing": "", "short": "1=abcd"} {
		t.Setenv("CATENARY_DATABASE_URL", "postgres://x")
		t.Setenv("CATENARY_MEDIA_DIR", t.TempDir())
		t.Setenv("CATENARY_MEDIA_URL_KEYS", keys)
		_, err := config.Load()
		if err == nil || !strings.Contains(err.Error(), "CATENARY_MEDIA_URL_KEYS") {
			t.Errorf("%s: Load = %v; want an error naming CATENARY_MEDIA_URL_KEYS", name, err)
		}
	}
	t.Setenv("CATENARY_MEDIA_DIR", "")
	t.Setenv("CATENARY_MEDIA_URL_KEYS", "")
	if _, err := config.Load(); err != nil {
		t.Errorf("no media dir needs no keys: %v", err)
	}
}

// syncURLs returns the attachment URLs /sync serves to ada.
func (r *uploadRig) syncURLs() []string {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/sync?after=0", nil)
	req.Header.Set("Authorization", "Bearer "+r.token)
	rec := httptest.NewRecorder()
	r.d.router.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	var page struct {
		Messages []servedURLs `json:"messages"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		r.t.Fatalf("decode /sync: %v", err)
	}
	var out []string
	for _, m := range page.Messages {
		for _, a := range m.Attachments {
			out = append(out, a.URL)
		}
	}
	return out
}
