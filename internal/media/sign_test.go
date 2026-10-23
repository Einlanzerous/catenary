package media

import (
	"bytes"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

var k1, k2 = strings.Repeat("11", 32), strings.Repeat("22", 32)

func mustSigner(t *testing.T, raw string) *Signer {
	t.Helper()
	s, err := ParseKeys(raw)
	if err != nil {
		t.Fatalf("ParseKeys(%q): %v", raw, err)
	}
	return s
}

func split(t *testing.T, u string) (key string, kid int, sig string) {
	t.Helper()
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	kid, _ = strconv.Atoi(p.Query().Get("kid"))
	return strings.TrimPrefix(p.Path, "/media/"), kid, p.Query().Get("sig")
}

func TestASignedURLVerifiesAndATamperedOneDoesNot(t *testing.T) {
	s := mustSigner(t, "1="+k1)
	key := strings.Repeat("ab", 32)
	u := s.URL(key)
	if !strings.HasPrefix(u, "/media/"+key+"?kid=1&sig=") {
		t.Fatalf("URL = %s", u)
	}
	k, kid, sig := split(t, u)
	if !s.Verify(k, kid, sig) {
		t.Fatal("a URL this signer made does not verify")
	}
	if s.URL(key) != u {
		t.Fatal("the URL is not a pure function of the key")
	}
	other := strings.Repeat("cd", 32)
	flipped := sig[:len(sig)-1] + map[bool]string{true: "B", false: "A"}[sig[len(sig)-1] == 'A']
	for name, ok := range map[string]bool{
		"another key":     s.Verify(other, kid, sig),
		"a flipped sig":   s.Verify(k, kid, flipped),
		"an unknown kid":  s.Verify(k, 9, sig),
		"an empty sig":    s.Verify(k, kid, ""),
		"a non-b64 sig":   s.Verify(k, kid, "!!!"),
		"a truncated sig": s.Verify(k, kid, sig[:10]),
	} {
		if ok {
			t.Errorf("%s verified", name)
		}
	}
}

func TestARotatedInKeySignsNewURLsAndTheOldKidStillVerifies(t *testing.T) {
	key := strings.Repeat("ab", 32)
	old := mustSigner(t, "1="+k1)
	rotated := mustSigner(t, "1="+k1+",2="+k2)

	_, kid, _ := split(t, rotated.URL(key))
	if kid != 2 {
		t.Fatalf("the newest key should sign; kid = %d", kid)
	}
	k, okid, osig := split(t, old.URL(key))
	if okid != 1 || !rotated.Verify(k, okid, osig) {
		t.Fatal("a URL signed under the older kid stopped verifying after rotation")
	}
	nk, nkid, nsig := split(t, rotated.URL(key))
	if old.Verify(nk, nkid, nsig) {
		t.Fatal("a signer without kid 2 verified a kid 2 URL")
	}
	if got := rotated.KIDs(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("KIDs = %v", got)
	}
}

func TestParseKeysRefusesAndNamesTheVariable(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":          "",
		"blank":          "   ",
		"short key":      "1=" + strings.Repeat("ab", 31),
		"not hex":        "1=" + strings.Repeat("zz", 32),
		"no kid":         k1,
		"zero kid":       "0=" + k1,
		"text kid":       "a=" + k1,
		"duplicate kid":  "1=" + k1 + ",1=" + k2,
		"trailing comma": "1=" + k1 + ",",
	} {
		_, err := ParseKeys(raw)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "CATENARY_MEDIA_URL_KEYS") {
			t.Errorf("%s: error does not name the variable: %v", name, err)
		}
		if raw != "" && strings.TrimSpace(raw) != "" && strings.Contains(err.Error(), k1) {
			t.Errorf("%s: error leaks key material: %v", name, err)
		}
	}
}

func TestContentType(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 20)...)
	jpg := append([]byte("\xff\xd8\xff\xe0"), bytes.Repeat([]byte{0}, 20)...)
	ogg := append([]byte("OggS\x00\x02"), bytes.Repeat([]byte{0}, 20)...)
	mp4 := append([]byte("\x00\x00\x00\x18ftypM4A "), bytes.Repeat([]byte{0}, 20)...)
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	html := []byte("<!DOCTYPE html><html><script>alert(1)</script>")
	for _, c := range []struct {
		name   string
		b      []byte
		kind   string
		ct     string
		inline bool
	}{
		{"png", png, "image", "image/png", true},
		{"jpeg", jpg, "image", "image/jpeg", true},
		{"ogg voice", ogg, "voice", "audio/ogg", true},
		{"ogg with no row", ogg, "", "application/octet-stream", false},
		{"m4a voice", mp4, "voice", "audio/mp4", true},
		{"m4a claimed as image", mp4, "image", "application/octet-stream", false},
		{"svg", svg, "image", "application/octet-stream", false},
		{"html", html, "image", "application/octet-stream", false},
		{"empty", nil, "voice", "application/octet-stream", false},
	} {
		ct, inline := ContentType(c.b, c.kind)
		if ct != c.ct || inline != c.inline {
			t.Errorf("%s = %s inline=%v; want %s inline=%v", c.name, ct, inline, c.ct, c.inline)
		}
	}
}
