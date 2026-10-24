package linkpreview

import (
	"errors"
	"io"
	"strings"

	"golang.org/x/net/html"
)

// meta is what the head of a page said, before sanitizing.
type meta struct {
	title, description, siteName, image string

	// overflow is true when the scan ended because one token passed maxTokenBytes.
	overflow bool
}

// maxTokenBytes bounds one HTML token, so a megabyte-long attribute cannot make
// the tokenizer buffer it whole.
const maxTokenBytes = 256 << 10

// extract reads r as a stream and stops at </head> or the first <body>, the
// end of the only part of a page that carries metadata. It never builds a tree.
// A read error or an oversized token ends the scan and keeps what was found:
// the cap on r is the caller's, and a page cut mid-head still yields its tags.
//
// Precedence per field is Open Graph, then Twitter card, then the plain HTML
// equivalent (title: <title>; description: meta name=description). The first
// occurrence of a key wins; og:image may legally repeat and the first is the
// primary.
func extract(r io.Reader) meta {
	z := html.NewTokenizer(r)
	z.SetMaxBuf(maxTokenBytes)

	got := map[string]string{}
	var titleText string
	var haveTitle bool

scan:
	for {
		switch z.Next() {
		case html.ErrorToken:
			break scan
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "body", "frameset":
				break scan
			case "title":
				if !haveTitle {
					if z.Next() == html.TextToken {
						titleText = string(z.Text())
						haveTitle = true
					}
				}
			case "meta":
				if !hasAttr {
					continue
				}
				var key, content string
				var hasContent bool
				for {
					k, v, more := z.TagAttr()
					switch string(k) {
					case "property", "name":
						if key == "" {
							key = strings.ToLower(strings.TrimSpace(string(v)))
						}
					case "content":
						content, hasContent = string(v), true
					}
					if !more {
						break
					}
				}
				if key != "" && hasContent {
					if _, seen := got[key]; !seen {
						got[key] = content
					}
				}
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "head" {
				break scan
			}
		}
	}

	first := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(got[k]); v != "" {
				return v
			}
		}
		return ""
	}
	m := meta{
		overflow:    errors.Is(z.Err(), html.ErrBufferExceeded),
		title:       first("og:title", "twitter:title"),
		description: first("og:description", "twitter:description", "description"),
		siteName:    first("og:site_name"),
		image:       first("og:image", "og:image:secure_url", "twitter:image", "twitter:image:src"),
	}
	if strings.TrimSpace(m.title) == "" {
		m.title = titleText
	}
	return m
}
