package linkpreview

import (
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	cases := []struct {
		name, in string
		max      int
		want     string
	}{
		{"plain", "Hello", 20, "Hello"},
		{"whitespace collapses", "  a \t\n b\u00A0\u2003c  ", 20, "a b c"},
		{"C0 controls removed", "a\x00b\x07c\x1bd", 20, "abcd"},
		{"C1 controls removed", "a\u0085b\u009bc", 20, "a bc"},
		{"bidi overrides removed", "\u202Eevil\u202C \u2066x\u2069", 20, "evil x"},
		{"every bidi char", "\u202A\u202B\u202C\u202D\u202E\u2066\u2067\u2068\u2069ok", 20, "ok"},
		{"direction marks and invisibles", "a\u200Eb\u200Fc\u061Cd\u200Be\u2060f\uFEFFg", 20, "abcdefg"},
		{"joiners are kept for emoji", "\U0001F468\u200D\U0001F469", 20, "\U0001F468\u200D\U0001F469"},
		{"zero width non-joiner is kept", "\u0646\u200C\u0645", 20, "\u0646\u200C\u0645"},
		{"soft hyphen and invisible operators", "re\u00ADsume \u2061\u2062\u2063\u2064x", 20, "resume x"},
		{"interlinear annotation marks", "\uFFF9a\uFFFAb\uFFFBc", 20, "abc"},
		{"tag characters (invisible text smuggling)", "ok\U000E0001\U000E0074\U000E0061\U000E007F", 20, "ok"},
		{"mongolian vowel separator and arabic letter mark", "a\u180Eb\u061Cc", 20, "abc"},
		{"combining marks are not format characters", "e\u0301", 20, "e\u0301"},
		{"invalid utf-8", "a\xffb", 20, "a\uFFFDb"},
		{"rune cap, not byte cap", strings.Repeat("\u00E9", 10), 4, "\u00E9\u00E9\u00E9\u00E9"},
		{"cap then trim", "ab cd", 3, "ab"},
		{"empty after cleaning", "\x00\u202E \t", 10, ""},
	}
	for _, c := range cases {
		if got := clean(c.in, c.max); got != c.want {
			t.Errorf("%s: clean(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
