package media

import (
	"net/http"
	"strings"
)

// ContentType decides what a blob is served as, from its first bytes and never
// from anything a client said (CANT-47 section 3). inline is false for anything
// that is not an image or audio type, which is then served as an attachment
// with application/octet-stream. SVG, which sniffs as text, is therefore never
// inline.
//
// kind is the uploads row's kind, or "" when unknown. Go's sniffer calls an
// Opus-in-Ogg voice note application/ogg and an m4a or webm recording video/*;
// for a row recorded as voice those are audio, and only then.
func ContentType(head []byte, kind string) (ctype string, inline bool) {
	t := http.DetectContentType(head)
	switch t {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return t, true
	}
	if strings.HasPrefix(t, "audio/") {
		return t, true
	}
	if kind == "voice" {
		// Go's sniffer only knows mp4 brands starting "mp4"; an m4a recording's
		// brand is "M4A ", so look for the ftyp box itself.
		if len(head) >= 12 && string(head[4:8]) == "ftyp" {
			return "audio/mp4", true
		}
		switch t {
		case "application/ogg":
			return "audio/ogg", true
		case "video/mp4":
			return "audio/mp4", true
		case "video/webm":
			return "audio/webm", true
		}
	}
	return "application/octet-stream", false
}
