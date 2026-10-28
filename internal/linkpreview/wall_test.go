package linkpreview

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// CANT-336. NOTHING HERE TOUCHES THE INTERNET, and no fixture was captured from a
// live site: every file in testdata/walls/ is hand-written from the shape of the
// page it stands for, and is served from a loopback httptest server. The site
// names resolve through the testEnv double to 127.0.0.1, the only address the
// test environment calls public; every other address meets the production judge.

// wallFixture is one row of the table the plan names (CANT-331, "Who owns the
// list, and how it is tested"). A row with an empty want must be a card.
type wallFixture struct {
	name     string // testdata/walls/<name>.html
	host     string // the host the URL is pasted with
	path     string // the pasted path and query
	redirect string // when set, the pasted path answers 302 to this path and query
	want     string // the wall_* detail, or "" for a card
}

var wallFixtures = []wallFixture{
	// Walls.
	{name: "instagram_reel_title_only", host: "www.instagram.com", path: "/reel/C9xYz12AbCd/", want: wallBrand},
	{name: "instagram_login_redirect", host: "www.instagram.com", path: "/reel/C9xYz12AbCd/", redirect: "/accounts/login/?next=%2Freel%2FC9xYz12AbCd%2F", want: wallLoginRedirect},
	{name: "cloudflare_just_a_moment_200", host: "shop.example", path: "/products/kettle", want: wallTitle},
	{name: "cloudflare_attention_required_200", host: "forum.example", path: "/t/some-thread/1234", want: wallTitle},
	{name: "facebook_log_into", host: "www.facebook.com", path: "/groups/1234567890/posts/9876543210/", want: wallTitle},
	{name: "linkedin_authwall_redirect", host: "www.linkedin.com", path: "/posts/someone_activity-7123456789-abcd", redirect: "/authwall?trk=public_post&sessionRedirect=x", want: wallLoginRedirect},
	{name: "generic_sign_in", host: "git.example.org", path: "/team/repo/issues/42", want: wallTitle},

	// Cards, each one a false positive the rule must not have.
	{name: "instagram_post", host: "www.instagram.com", path: "/p/C8aBcDeFgHi/"},
	{name: "nebula_mustard", host: "nebula.tv", path: "/videos/mustard-the-aircraft"},
	{name: "youtube_video", host: "www.youtube.com", path: "/watch?v=dQw4w9WgXcQ"},
	{name: "logging_in_rust", host: "blog.example.net", path: "/2026/logging-in-rust"},
	{name: "router_how_to_log_in", host: "help.example.com", path: "/articles/router-login"},
	{name: "just_a_moment_of_silence", host: "essays.example", path: "/moment-of-silence"},
}

// TestWallFixtures serves each fixture through the real Guarded, so the rule is
// tested where it runs: after the page, before the image. A wall must cost no
// image request, which the image handler counts.
func TestWallFixtures(t *testing.T) {
	for _, f := range wallFixtures {
		t.Run(f.name, func(t *testing.T) {
			page, err := os.ReadFile(filepath.Join("testdata", "walls", f.name+".html"))
			if err != nil {
				t.Fatal(err)
			}
			var images atomic.Int32
			pastedPath, _, _ := strings.Cut(f.path, "?")
			_, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/card.png":
					images.Add(1)
					w.Header().Set("Content-Type", "image/png")
					w.Write(tinyPNG)
				case f.redirect != "" && r.URL.Path == pastedPath:
					http.Redirect(w, r, f.redirect, http.StatusFound)
				default:
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.Write(page)
				}
			}))
			env, _ := testEnv([]string{"127.0.0.1"}, map[string][][]string{f.host: {{"127.0.0.1"}}})
			g := newGuarded(testConfig(), env)

			p, err := g.Preview(context.Background(), "http://"+f.host+":"+portOf(strings.TrimPrefix(base, "http://"))+f.path)
			if f.want != "" {
				if ClassOf(err) != ClassEmpty || DetailOf(err) != f.want {
					t.Fatalf("p = %+v, err = %v; want empty (%s)", p, err, f.want)
				}
				if n := images.Load(); n != 0 {
					t.Errorf("a wall fetched its og:image %d times; want 0", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v; want a card", err)
			}
			if p.Title == "" || len(p.Image) == 0 || images.Load() != 1 {
				t.Errorf("card = %+v, image requests = %d; want a title and its image", p, images.Load())
			}
		})
	}
}

// Every fixture on disk has a row and every row a fixture: an entry is added
// only with a fixture, and a fixture nobody serves proves nothing.
func TestEveryWallFixtureHasARow(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "walls", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk, inTable []string
	for _, f := range files {
		onDisk = append(onDisk, strings.TrimSuffix(filepath.Base(f), ".html"))
	}
	for _, f := range wallFixtures {
		inTable = append(inTable, f.name)
	}
	sort.Strings(onDisk)
	sort.Strings(inTable)
	if strings.Join(onDisk, ",") != strings.Join(inTable, ",") {
		t.Errorf("fixtures on disk %v; rows %v", onDisk, inTable)
	}
}

// TestWallDetail is the rule on its own, for the edges a fixture would only
// repeat: the separators, the folding, the templated phrases, the brand
// sources, and the redirect signal's boundaries.
func TestWallDetail(t *testing.T) {
	cases := []struct {
		name, pasted, final, title, site, want string
	}{
		// The plan's own examples.
		{"bare brand", "https://www.instagram.com/reel/x/", "", "Instagram", "", wallBrand},
		{"phrase bullet brand", "https://www.instagram.com/reel/x/", "", "Login • Instagram", "", wallTitle},
		{"log into brand", "https://www.facebook.com/x", "", "Log into Facebook", "", wallTitle},
		{"just a moment", "https://shop.example/x", "", "Just a moment...", "", wallTitle},
		{"attention required pipe cloudflare", "https://shop.example/x", "", "Attention Required! | Cloudflare", "", wallTitle},
		{"title en dash brand is a card", "https://nebula.tv/videos/x", "", "Mustard – Nebula", "Nebula", ""},
		{"author on brand is a card", "https://www.instagram.com/p/x/", "", `Bergen Hill Co-op on Instagram: "..."`, "Instagram", ""},
		{"logging in rust", "https://blog.example.net/x", "", "Logging in Rust", "", ""},
		{"how to log in pipe brand", "https://help.example.com/x", "", "How to log in to your router | Example", "Example", ""},

		// Every separator splits, and only with a space on each side.
		{"pipe", "https://x.example/", "", "Sign in | X", "", wallTitle},
		{"middle dot", "https://x.example/", "", "Sign in · X", "", wallTitle},
		{"bullet", "https://x.example/", "", "Sign in • X", "", wallTitle},
		{"hyphen", "https://x.example/", "", "Sign in - X", "", wallTitle},
		{"en dash", "https://x.example/", "", "Sign in – X", "", wallTitle},
		{"em dash", "https://x.example/", "", "Sign in — X", "", wallTitle},
		{"three segments", "https://www.facebook.com/x", "", "Facebook - log in or sign up | Facebook", "", wallTitle},
		{"unspaced hyphen does not split", "https://x.example/", "", "Sign-in X", "", ""},
		{"unspaced pipe does not split", "https://x.example/", "", "Sign in|X", "", ""},
		{"brand twice is still the brand", "https://www.youtube.com/", "", "YouTube | YouTube", "", wallBrand},

		// Folding: case, the ellipsis character, trailing dots and spaces.
		{"upper case", "https://x.example/", "", "LOG IN", "", wallTitle},
		{"ellipsis character", "https://x.example/", "", "Just a moment…", "", wallTitle},
		{"please wait dots", "https://x.example/", "", "Please wait.....", "", wallTitle},
		{"a segment of only dots says nothing", "https://www.instagram.com/", "", "Instagram | ...", "", wallBrand},
		{"only dots is no wall", "https://x.example/", "", "...", "", ""},
		{"empty title is no wall", "https://www.instagram.com/", "", "", "Instagram", ""},
		{"question mark kept", "https://x.example/", "", "Are you a robot?", "", wallTitle},
		{"question mark missing is not the phrase", "https://x.example/", "", "Are you a robot", "", ""},

		// Phrases are whole segments; a phrase plus anything but a brand is a card.
		{"phrase as a prefix", "https://essays.example/", "", "Just a Moment of Silence…", "", ""},
		{"phrase then other words", "https://x.example/", "", "Sign in with your library card", "", ""},
		{"login as a word in a title", "https://x.example/", "", "Login flows that respect people", "", ""},
		{"phrase to brand", "https://www.instagram.com/", "", "Log in to Instagram", "", wallTitle},
		{"phrase to other", "https://www.instagram.com/", "", "Log in to your router", "", ""},
		{"brand phrase", "https://www.instagram.com/", "", "Instagram login", "", wallTitle},
		{"other phrase", "https://www.instagram.com/", "", "Router login", "", ""},
		{"phrase brand from site name", "https://x.example/", "", "Sign in to Bergen Hill", "Bergen Hill", wallTitle},

		// Brand tokens: every host label but the last and www, and og:site_name.
		{"subdomain label", "https://news.ycombinator.com/item?id=1", "", "Hacker News", "", ""},
		{"subdomain label is a brand", "https://news.ycombinator.com/item?id=1", "", "News", "", wallBrand},
		{"top-level label is no brand", "https://www.instagram.com/", "", "Com", "", ""},
		{"www is no brand", "https://www.instagram.com/", "", "WWW", "", ""},
		{"site name is a brand", "https://blog.example.net/", "", "Field Notes", "Field Notes", wallBrand},
		{"final host is a brand", "https://t.co/abc", "https://www.instagram.com/reel/x/", "Instagram", "", wallBrand},
		{"brand plus words is a card", "https://www.instagram.com/", "", "Instagram Reels", "", ""},
		{"brand contained in a word", "https://www.instagram.com/", "", "Instagrammable", "", ""},
		{"ip literal gives no brand", "https://127.0.0.1/", "", "127", "", ""},

		// The redirect signal: only after a redirect, only on a login path, and
		// only at a path boundary.
		{"redirect to accounts login", "https://www.instagram.com/reel/x/", "https://www.instagram.com/accounts/login/?next=%2F", "Anything at all", "", wallLoginRedirect},
		{"redirect to login.php", "https://www.facebook.com/x", "https://www.facebook.com/login.php?next=x", "Some page", "", wallLoginRedirect},
		{"redirect to bare login", "https://x.example/a", "https://x.example/login", "Some page", "", wallLoginRedirect},
		{"redirect to upper-case path", "https://x.example/a", "https://x.example/SignIn/", "Some page", "", wallLoginRedirect},
		{"redirect to sign-in", "https://x.example/a", "https://x.example/sign-in", "Some page", "", wallLoginRedirect},
		{"redirect to account login", "https://x.example/a", "https://x.example/account/login", "Some page", "", wallLoginRedirect},
		{"redirect to authwall", "https://www.linkedin.com/posts/x", "https://www.linkedin.com/authwall?trk=x", "Some page", "", wallLoginRedirect},
		{"redirect to checkpoint", "https://www.facebook.com/x", "https://www.facebook.com/checkpoint/1501092823525282/", "Some page", "", wallLoginRedirect},
		{"redirect to another host's login", "https://bit.example/s", "https://idp.example/login", "Some page", "", wallLoginRedirect},
		{"redirect wins over a card title", "https://www.instagram.com/reel/x/", "https://www.instagram.com/accounts/login/", "Bergen Hill Co-op on Instagram", "", wallLoginRedirect},
		{"login path pasted directly is no redirect", "https://x.example/login", "", "Some page", "", ""},
		{"login path pasted directly, title wall", "https://www.instagram.com/accounts/login/", "", "Login • Instagram", "", wallTitle},
		{"loginfo is no login path", "https://x.example/a", "https://x.example/loginfo", "Some page", "", ""},
		{"logins is no login path", "https://x.example/a", "https://x.example/logins-of-2026", "Some page", "", ""},
		{"login deeper in the path", "https://x.example/a", "https://x.example/docs/login", "Some page", "", ""},
		{"login in the query", "https://x.example/a", "https://x.example/b?next=/login", "Some page", "", ""},
	}
	for _, c := range cases {
		pasted, err := url.Parse(c.pasted)
		if err != nil {
			t.Fatal(err)
		}
		final := pasted
		if c.final != "" {
			if final, err = url.Parse(c.final); err != nil {
				t.Fatal(err)
			}
		}
		if got := wallDetail(pasted, final, clean(c.title, maxTitleRunes), clean(c.site, maxSiteNameRunes)); got != c.want {
			t.Errorf("%s: wallDetail(%q, %q, %q, %q) = %q; want %q", c.name, c.pasted, c.final, c.title, c.site, got, c.want)
		}
	}
}

// A wall's error text is the class and its fixed token, never the URL, so the
// endpoint's log line stays free of the pasted path and query.
func TestWallErrorCarriesNoURL(t *testing.T) {
	pasted, _ := url.Parse("https://www.instagram.com/reel/secret-token/?igsh=hunter2")
	final, _ := url.Parse("https://www.instagram.com/accounts/login/?next=%2Freel%2Fsecret-token%2F")
	err := newErr(ClassEmpty, wallDetail(pasted, final, "Login • Instagram", "Instagram"), nil)
	if s := err.Error(); strings.Contains(s, "secret") || strings.Contains(s, "hunter2") || s != "linkpreview: empty (wall_login_redirect)" {
		t.Errorf("err = %q", s)
	}
}
