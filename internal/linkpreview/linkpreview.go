// Package linkpreview fetches the page behind a URL a member is composing and
// reduces it to a card: a title, a description, a site name and, optionally, the
// bytes of one image. CANT-308, row 0; nothing in the binary calls it yet.
//
// THIS IS THE ONE COMPONENT THAT CONNECTS TO ADDRESSES AN ATTACKER CHOSE. A
// member (or anyone who can get a link in front of one) names a host, and this
// package opens a socket to it from inside the estate's network, where
// construct_net, every Docker bridge and the cloud metadata address
// 169.254.169.254 are all one hop away. A hole here is a server-side request
// forgery, so the defense is built to have exactly one place it can be wrong:
//
//   - THE ADDRESS IS JUDGED AT CONNECT TIME. A net.Dialer.ControlContext sees the
//     remote address of every socket the kernel is about to connect, after name
//     resolution, and refuses it unless it is a global unicast address on the
//     policy in address.go. There is no resolve-then-check step to race, so DNS
//     rebinding and a redirect to a rebinding name die the same way a literal
//     does. Nothing in this package is allowed to dial by any other route.
//   - EVERY HOP USES THAT DIALER. Redirects are followed by the same client, at
//     most three of them, http/https only, ports 80 and 443 only, never from
//     https down to http. Keep-alives are off so no connection outlives a policy
//     decision, and Proxy is nil so HTTP_PROXY in the environment cannot carry the
//     request around the dialer.
//   - EVERYTHING IS BOUNDED: time, bytes after decompression, redirects, headers.
//
// The seam for a future estate unfurl service (IDEA-66) is Previewer: a URL in,
// card fields and optional image bytes out. Guarded is the first implementation
// and the one that fetches in-process; a second would sit behind the same
// interface and the caller would not change.
package linkpreview

import (
	"context"
	"time"
)

// Previewer turns a URL into card fields and, when the page names one and it
// could be fetched, the raw bytes of the card image.
//
// An implementation must apply the address, redirect and size policy of this
// package or an equivalent one; a caller treats every error as "no card" and
// uses ClassOf only to log. It never reports the URL's path or query.
type Previewer interface {
	Preview(ctx context.Context, rawURL string) (*Preview, error)
}

// Preview is a card. Every text field is already sanitized and length-capped.
type Preview struct {
	// URL is the canonical form that was fetched: lowercase scheme, ASCII (IDNA)
	// host, no fragment. The card shows the host of this, never SiteName.
	URL string

	Title       string
	Description string
	SiteName    string

	// Image is the card image exactly as the target served it, at most
	// Config.MaxImageBytes. It is UNTRUSTED BYTES: the caller must run it through
	// media.IngestImage, which sniffs the real type and re-encodes it; nothing here
	// has looked at them. Nil when the page named no image or it could not be had.
	Image []byte

	// ImageClass is why Image is nil although the page named one (a Class value,
	// empty when there was no image to fetch or it was fetched). It is for the log
	// line only: a blocked image address is worth noticing.
	ImageClass Class
}

// Config bounds one fetch. DefaultConfig is the plan's table (CANT-308,
// fetch-policy); the endpoint row maps CATENARY_LINKPREVIEW_* onto it.
type Config struct {
	ConnectTimeout time.Duration // per socket
	PageTimeout    time.Duration // the whole page fetch, body included
	ImageTimeout   time.Duration // the whole image fetch, body included
	TotalTimeout   time.Duration // the whole Preview call

	MaxPageBytes  int64 // read after decompression; a gzip bomb is cut here
	MaxImageBytes int64
	MaxRedirects  int

	UserAgent string
}

// DefaultConfig returns the values the plan sets.
func DefaultConfig() Config {
	return Config{
		ConnectTimeout: 3 * time.Second,
		PageTimeout:    6 * time.Second,
		ImageTimeout:   4 * time.Second,
		TotalTimeout:   10 * time.Second,
		MaxPageBytes:   2 << 20,
		MaxImageBytes:  5 << 20,
		MaxRedirects:   3,
		UserAgent:      "Catenary-LinkPreview/1",
	}
}

const (
	maxTitleRunes       = 200
	maxDescriptionRunes = 300
	maxSiteNameRunes    = 80
)
