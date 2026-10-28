package linkpreview

// CANT-336 (CANT-331 rulings 2 and 3, option A each): the wall rule. A login
// wall or a bot check answers 200 with a page whose title names only the site
// or the wall itself ("Instagram", "Login • Instagram", "Just a moment..."),
// and a card built from it would freeze that title, and the site's logo, into
// a message. Such a page is no card: the existing ClassEmpty error with a
// wall_* detail, so the endpoint answers its uniform 204, remembers it as a
// failure for NegativeCacheTTL and writes nothing to uploads.
//
// The rule is deliberately narrow. A title is a wall only when EVERY segment of
// it is a brand token or a wall phrase, never when it merely CONTAINS one: a
// real post's title always has a segment that says something the host does
// not, which is what keeps `Bergen Hill Co-op on Instagram: "..."`, `Logging in
// Rust` and `How to log in to your router | Example` cards. Its one stated false
// positive is a page whose whole title is its own site's name or a sign-in
// phrase (an instagram.com page really titled "Instagram"); the plan accepts
// losing that card, whose title says nothing the host line does not.
//
// It runs in Guarded.Preview between the page and the image, so a wall costs no
// image fetch (a wall's og:image is typically the brand logo). It is a pure
// function of what was already fetched: it opens nothing, and nothing about it
// can widen what the fetcher connects to.
//
// The lists are owned by the CANT project, in code, with no configuration
// override. An entry is added only with a fixture in testdata/walls/ and a row
// in TestWallFixtures; a reported false positive is answered the same way, as a
// must-be-a-card fixture.

import (
	"net/netip"
	"net/url"
	"strings"
)

// The detail tokens a wall carries on its ClassEmpty error. They are what the
// endpoint logs as detail=, and never contain the URL.
const (
	// wallLoginRedirect: the page was reached by a redirect that landed on a
	// login path. Checked first.
	wallLoginRedirect = "wall_login_redirect"
	// wallTitle: every title segment is a brand token or a wall phrase, and at
	// least one is a phrase ("Login • Instagram", "Just a moment...").
	wallTitle = "wall_title"
	// wallBrand: every title segment is a brand token ("Instagram").
	wallBrand = "wall_brand"
)

// wallSeparators split a title into segments. Each is surrounded by spaces, so
// a hyphenated word ("Co-op", "sign-in") is never split. clean has already
// collapsed every run of whitespace to one space.
var wallSeparators = []string{" | ", " · ", " • ", " - ", " – ", " — "}

// wallSplitter maps every separator onto NUL, which clean guarantees never
// appears in a title (it is a control character).
var wallSplitter = func() *strings.Replacer {
	var pairs []string
	for _, s := range wallSeparators {
		pairs = append(pairs, s, "\x00")
	}
	return strings.NewReplacer(pairs...)
}()

// wallPhrases are the folded phrases a wall's title is made of. Each also
// matches in the templated forms "<phrase> to <brand>", "<phrase> <brand>" and
// "<brand> <phrase>" (wallSegment).
var wallPhrases = []string{
	"log in",
	"login",
	"log into",
	"sign in",
	"sign up",
	"log in or sign up",
	"just a moment",
	"please wait",
	"one more step",
	"attention required!",
	"access denied",
	"security check",
	"robot check",
	"are you a robot?",
	"verify you are human",
	"cloudflare",
}

// wallLoginPaths are the path prefixes a redirect may land on to make the page
// a wall. Compared against the lowercased final path, and only at a boundary
// (wallPathHas): /login, /login/ and /login.php are login paths, /logins-2026
// and /loginfo are not.
var wallLoginPaths = []string{
	"/login",
	"/signin",
	"/sign-in",
	"/accounts/login",
	"/account/login",
	"/authwall",
	"/checkpoint",
}

// wallDetail returns the wall_* detail token when the fetched page is a login
// wall or a bot check, and "" when it may be a card. pasted is the URL Preview
// was asked for and final the one the page came from; title and siteName are
// the page's already-cleaned title and og:site_name.
func wallDetail(pasted, final *url.URL, title, siteName string) string {
	if final.String() != pasted.String() && wallPathHas(final.Path) {
		return wallLoginRedirect
	}

	var segments []string
	for _, s := range strings.Split(wallSplitter.Replace(title), "\x00") {
		// A segment that folds to nothing ("Instagram | ...") says nothing the
		// host does not, so it neither makes nor unmakes a wall.
		if f := wallFold(s); f != "" {
			segments = append(segments, f)
		}
	}
	if len(segments) == 0 {
		return ""
	}

	brands := wallBrands(pasted, final, siteName)
	phrase := false
	for _, s := range segments {
		// A phrase that is also a brand ("Login" on login.example.com) still
		// counts as a phrase: wall_brand is only for a title with none.
		isPhrase := wallSegmentIsPhrase(s, brands)
		if !isPhrase && !brands[s] {
			// One segment that says something the host does not: a card.
			return ""
		}
		phrase = phrase || isPhrase
	}
	if phrase {
		return wallTitle
	}
	return wallBrand
}

// wallFold is the comparison form: lowercased, the ellipsis character spelled
// as three dots, and trailing dots and spaces trimmed, so "Just a moment…",
// "Just a moment..." and "just a moment" are one string.
func wallFold(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, "…", "..."))
	return strings.TrimRight(strings.TrimSpace(s), ". ")
}

// wallBrands are the brand tokens for one page: every label of the pasted and
// the final host except the last one and "www", plus the folded og:site_name.
// There is no per-site list: instagram.com yields "instagram". An IP literal
// has no labels worth the name and yields none.
func wallBrands(pasted, final *url.URL, siteName string) map[string]bool {
	brands := map[string]bool{}
	for _, u := range []*url.URL{pasted, final} {
		host := strings.ToLower(u.Hostname())
		if _, err := netip.ParseAddr(host); err == nil {
			continue
		}
		labels := strings.Split(strings.TrimSuffix(host, "."), ".")
		for _, l := range labels[:len(labels)-1] {
			if l != "" && l != "www" {
				brands[l] = true
			}
		}
	}
	if f := wallFold(siteName); f != "" {
		brands[f] = true
	}
	return brands
}

// wallSegmentIsPhrase reports whether a folded segment is a wall phrase, or one
// in a templated form with a brand token.
func wallSegmentIsPhrase(s string, brands map[string]bool) bool {
	for _, p := range wallPhrases {
		if s == p {
			return true
		}
		if rest, ok := strings.CutPrefix(s, p+" "); ok {
			if brands[rest] {
				return true // "<phrase> <brand>": "log into facebook"
			}
			if b, ok := strings.CutPrefix(rest, "to "); ok && brands[b] {
				return true // "<phrase> to <brand>": "log in to instagram"
			}
		}
		if b, ok := strings.CutSuffix(s, " "+p); ok && brands[b] {
			return true // "<brand> <phrase>": "instagram login"
		}
	}
	return false
}

// wallPathHas reports whether a final path is one of wallLoginPaths, matched as
// a prefix that ends at the path's end, a "/" or a ".".
func wallPathHas(path string) bool {
	path = strings.ToLower(path)
	for _, p := range wallLoginPaths {
		rest, ok := strings.CutPrefix(path, p)
		if ok && (rest == "" || rest[0] == '/' || rest[0] == '.') {
			return true
		}
	}
	return false
}
