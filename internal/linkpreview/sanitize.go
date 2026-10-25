package linkpreview

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// clean makes page-chosen text safe to render as a plain string: invalid UTF-8
// replaced, whitespace (including the whitespace controls) collapsed to single
// spaces, every other control character (Cc) and every FORMAT character (Cf)
// removed, the two noncharacters U+FFFE and U+FFFF removed, then cut to max
// runes.
//
// Cf is stripped as a CATEGORY, not as a list. It holds the bidirectional
// overrides and isolates (U+202A-202E, U+2066-2069), the direction marks
// (U+200E, U+200F, U+061C), the zero width space, word joiner and BOM, the
// soft hyphen, the invisible operators (U+2061-2064), the interlinear
// annotation marks (U+FFF9-FFFB) and the tag characters (U+E0000-E007F): each
// renders as nothing or lets a title read as a different word than it is, and
// a list of them is what grows one entry per incident. The joiners U+200C and
// U+200D are the one exception, KEPT because emoji sequences and several
// scripts need them. A control character is how a title writes into a log line.
func clean(s string, max int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\u200C', r == '\u200D':
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == '\uFFFE', r == '\uFFFF':
			continue
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(out) > max {
		rs := []rune(out)
		out = strings.TrimSpace(string(rs[:max]))
	}
	return out
}
