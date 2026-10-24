package linkpreview

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// clean makes page-chosen text safe to render as a plain string: invalid UTF-8
// replaced, whitespace (including the whitespace controls) collapsed to single
// spaces, every other C0/C1 control character and the Unicode bidirectional
// override and isolate characters (U+202A-202E, U+2066-2069), the direction marks
// (U+200E, U+200F, U+061C) and the invisible U+200B, U+2060 and U+FEFF removed, then cut
// to max runes. The bidi characters are what let a title read as a different
// word than it is; a control character is how a title writes into a log line.
func clean(s string, max int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case isBidiControl(r), isInvisible(r):
			continue
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		case unicode.IsControl(r), r == '\uFFFE', r == '\uFFFF':
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

func isBidiControl(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069: // overrides, embeddings, isolates
		return true
	case r == 0x200E, r == 0x200F, r == 0x061C: // LRM, RLM, ALM: invisible direction marks
		return true
	}
	return false
}

// isInvisible reports the format characters that render as nothing and carry no
// meaning in a plain-string card: zero width space, word joiner, the BOM. U+200C
// and U+200D (the joiners) are KEPT: emoji sequences and several scripts need them.
func isInvisible(r rune) bool {
	return r == 0x200B || r == 0x2060 || r == 0xFEFF
}
