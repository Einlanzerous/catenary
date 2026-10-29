package linkpreview

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// CANT-337. NOTHING HERE TOUCHES THE INTERNET: TestFinalHost is a pure function
// over parsed URLs, and TestGuardedFinalHostThroughARedirect serves every name
// from one loopback httptest server, the names resolving through the testEnv
// double to 127.0.0.1.

// TestFinalHost is the table of the plan's redirect-host section (CANT-331),
// with ruling 1 option A's answers (registrable domain), plus the
// canonicalization cases its Done-when names. want is the stored final host,
// "" for hidden; refused means no card (detail final_host).
func TestFinalHost(t *testing.T) {
	long := strings.Repeat("a", 64)
	cases := []struct {
		name          string
		pasted, final string
		want          string
		refused       bool
	}{
		// The redirect-host table, option A column.
		{name: "shortener to another site", pasted: "https://youtu.be/x-wqMcYMNdg", final: "https://www.youtube.com/watch?v=x-wqMcYMNdg", want: "www.youtube.com"},
		{name: "go. hop to the apex", pasted: "https://go.nebula.tv/mustard", final: "https://nebula.tv/videos/mustard"},
		{name: "www. hop to the apex", pasted: "https://www.instagram.com/p/x/", final: "https://instagram.com/p/x/"},
		{name: "m. to www. on one domain", pasted: "https://m.example.com/a", final: "https://www.example.com/a"},
		{name: "two subdomains of one domain", pasted: "https://a.example.com/", final: "https://b.example.com/"},
		{name: "tenant to tenant on a shared platform", pasted: "https://alice.github.io/", final: "https://bob.github.io/", want: "bob.github.io"},
		{name: "platform to a tenant", pasted: "https://github.io/", final: "https://alice.github.io/", want: "alice.github.io"},
		{name: "name to an IPv6 literal", pasted: "https://example.com/", final: "https://[2001:db8::1]/", want: "[2001:db8::1]"},

		// Nothing redirected.
		{name: "no redirect", pasted: "https://www.youtube.com/watch?v=x", final: "https://www.youtube.com/watch?v=x"},
		{name: "same host, another path", pasted: "https://example.com/s/1", final: "https://example.com/post/1"},

		// Canonicalization of the stored host.
		{name: "IDN final host is stored as punycode", pasted: "https://example.com/", final: "https://bücher.example/", want: "xn--bcher-kva.example"},
		{name: "case and a trailing dot", pasted: "https://short.example/x", final: "https://Example.COM./x", want: "example.com"},
		{name: "case and a trailing dot on one site", pasted: "https://example.com/x", final: "https://Example.COM./y"},
		{name: "IPv6 literal is lowercased and keeps its brackets", pasted: "https://example.com/", final: "http://[2001:DB8::1]/", want: "[2001:db8::1]"},
		{name: "IPv6 literal is compressed", pasted: "https://example.com/", final: "http://[2001:0db8:0:0:0:0:0:1]/", want: "[2001:db8::1]"},
		{name: "IPv4 literal is shown", pasted: "https://example.com/", final: "http://93.184.216.34/", want: "93.184.216.34"},
		{name: "two IPv4 literals are compared whole", pasted: "http://1.2.216.34/", final: "http://93.184.216.34/", want: "93.184.216.34"},
		{name: "one IPv4 literal is one site", pasted: "http://93.184.216.34/a", final: "http://93.184.216.34/b"},
		{name: "the port is dropped", pasted: "https://youtu.be/x", final: "https://www.youtube.com:443/watch", want: "www.youtube.com"},
		{name: "a port alone is no hop", pasted: "http://example.com/", final: "https://example.com:443/"},

		// Hosts the pattern refuses: no card, but only when the host would be stored.
		{name: "a 64-byte label", pasted: "https://example.com/", final: "https://" + long + ".example/", refused: true},
		{name: "a single-label host", pasted: "https://example.com/", final: "http://intranet/", refused: true},
		{name: "an IPv6 literal with a zone", pasted: "https://example.com/", final: "http://[fe80::1%25eth0]/", refused: true},
		{name: "a refused host that is never stored is no refusal", pasted: "https://" + long + ".example/a", final: "https://" + long + ".example/b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pasted, err := url.Parse(c.pasted)
			if err != nil {
				t.Fatal(err)
			}
			final, err := url.Parse(c.final)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := finalHost(pasted, final)
			if c.refused {
				if ok {
					t.Fatalf("finalHost = %q, ok; want refused", got)
				}
				return
			}
			if !ok || got != c.want {
				t.Fatalf("finalHost = %q, %v; want %q, true", got, ok, c.want)
			}
			// What is stored has to be what the wire, the database and the decoders accept.
			if got != "" && (len(got) > finalHostMaxLen || !finalHostPattern.MatchString(got)) {
				t.Errorf("stored %q does not match the wire pattern", got)
			}
		})
	}
}

// The pattern here must be the schema's, character for character, or Go and
// the decoders could disagree on what a final host is. Read from the schema
// file itself, so an edit there fails here.
func TestFinalHostPatternIsTheSchemas(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schema", "catenary.wire.v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Pattern   string `json:"pattern"`
				MaxLength int    `json:"maxLength"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	want, ok := schema.Defs["LinkAttachment"].Properties["final_host"]
	if !ok || want.Pattern == "" {
		t.Fatal("the schema has no LinkAttachment.final_host pattern")
	}
	if finalHostPattern.String() != want.Pattern {
		t.Errorf("pattern = %s, want the schema's %s", finalHostPattern, want.Pattern)
	}
	if finalHostMaxLen != want.MaxLength {
		t.Errorf("max length = %d, want the schema's %d", finalHostMaxLen, want.MaxLength)
	}
}

// TestGuardedFinalHostThroughARedirect runs the real Guarded through a real 301
// on the loopback harness, with the names resolved by testEnv: a cross-site hop
// sets FinalHost, a same-site hop leaves it empty, and a hop to a host the
// pattern refuses is no card and fetches no image.
func TestGuardedFinalHostThroughARedirect(t *testing.T) {
	long := strings.Repeat("a", 64) + ".example"
	var images atomic.Int32
	_, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		port := portOf(r.Host)
		switch {
		case r.URL.Path == "/card.png":
			images.Add(1)
			w.Header().Set("Content-Type", "image/png")
			w.Write(tinyPNG)
		case strings.HasPrefix(r.Host, "youtu.be:"):
			http.Redirect(w, r, "http://www.youtube.com:"+port+"/watch?v=x-wqMcYMNdg", http.StatusMovedPermanently)
		case strings.HasPrefix(r.Host, "go.nebula.tv:"):
			http.Redirect(w, r, "http://nebula.tv:"+port+"/videos/mustard", http.StatusMovedPermanently)
		case strings.HasPrefix(r.Host, "short.example:"):
			http.Redirect(w, r, "http://"+long+":"+port+"/", http.StatusMovedPermanently)
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<head><title>The video</title><meta property="og:image" content="/card.png"></head>`)
		}
	}))
	port := portOf(strings.TrimPrefix(base, "http://"))
	loop := [][]string{{"127.0.0.1"}}
	env, _ := testEnv([]string{"127.0.0.1"}, map[string][][]string{
		"youtu.be": loop, "www.youtube.com": loop,
		"go.nebula.tv": loop, "nebula.tv": loop,
		"short.example": loop, long: loop,
	})
	g := newGuarded(testConfig(), env)

	p, err := g.Preview(context.Background(), "http://youtu.be:"+port+"/x-wqMcYMNdg")
	if err != nil {
		t.Fatal(err)
	}
	if p.FinalHost != "www.youtube.com" {
		t.Errorf("youtu.be: FinalHost = %q, want www.youtube.com", p.FinalHost)
	}

	p, err = g.Preview(context.Background(), "http://go.nebula.tv:"+port+"/mustard")
	if err != nil {
		t.Fatal(err)
	}
	if p.FinalHost != "" || !strings.Contains(p.FinalURL, "//nebula.tv:") {
		t.Errorf("go.nebula.tv: FinalHost = %q (FinalURL %q), want empty after a same-site hop", p.FinalHost, p.FinalURL)
	}

	images.Store(0)
	p, err = g.Preview(context.Background(), "http://short.example:"+port+"/x")
	if ClassOf(err) != ClassEmpty || DetailOf(err) != detailFinalHost {
		t.Fatalf("refused host: p = %+v, err = %v; want empty (final_host)", p, err)
	}
	if n := images.Load(); n != 0 {
		t.Errorf("a refused final host fetched its og:image %d times; want 0", n)
	}
}
