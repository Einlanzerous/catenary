package main

// CANT-322 (CANT-308 row 3) — the service behind POST /link-previews. Kept out
// of setup() the way imageupload.go is, and built only when the endpoint is on.
//
// The fetching is internal/linkpreview's (CANT-319); this file adds what the
// endpoint owes around it, in the order the plan fixes:
//
//	1  the URL is checked for syntax (400), opening nothing;
//	2  the account's token bucket (429);
//	3  the cache, which opens no socket on a hit but STILL MINTS A FRESH HANDLE,
//	   because a handle is single-use and uploader-bound and two members pasting
//	   the same link must not share one;
//	4  the server-wide concurrency cap (429), then the fetch;
//	5  the image through media.IngestImage and Store.Put, then the handle.
//
// Every fetch failure is the same answer to the caller (nil Card) and a log line
// here. The URL's path and query are never logged: a pasted link can carry a
// token, and the class is all an operator needs.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/api"
	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/linkpreview"
	"github.com/magos/catenary/internal/media"
	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// newPreviewer builds the fetcher setup() uses when previews are on. A variable
// so a test through setup() can substitute a double: CI never makes a real
// outbound request, and "no socket opens with the switch off" is asserted by a
// double that was never asked for.
var newPreviewer = func(c linkpreview.Config) linkpreview.Previewer { return linkpreview.New(c) }

// linkCache is one cache entry: either a card (the upload row without its
// uploader, pointing at blobs already in the store) or a remembered failure.
type linkCache struct {
	card  *store.NewUpload
	class linkpreview.Class
}

const linkCacheEntries = 1024

type linkService struct {
	logger *slog.Logger
	fetch  linkpreview.Previewer
	media  media.Store
	st     *store.Store
	derive func(storageKey string) string

	limiter  *linkpreview.Limiter
	cache    *linkpreview.Cache[linkCache]
	inflight chan struct{}

	ttl, negativeTTL time.Duration
	now              func() time.Time
}

func newLinkService(cfg config.LinkPreviews, logger *slog.Logger, ms media.Store, st *store.Store, derive func(string) string) *linkService {
	d := config.DefaultLinkPreviews()
	or := func(v, def int) int {
		if v <= 0 {
			return def
		}
		return v
	}
	orD := func(v, def time.Duration) time.Duration {
		if v <= 0 {
			return def
		}
		return v
	}
	fc := linkpreview.DefaultConfig()
	fc.ConnectTimeout = orD(cfg.ConnectTimeout, d.ConnectTimeout)
	fc.PageTimeout = orD(cfg.PageTimeout, d.PageTimeout)
	fc.ImageTimeout = orD(cfg.ImageTimeout, d.ImageTimeout)
	fc.TotalTimeout = orD(cfg.TotalTimeout, d.TotalTimeout)
	if cfg.MaxPageBytes > 0 {
		fc.MaxPageBytes = cfg.MaxPageBytes
	}
	if cfg.MaxImageBytes > 0 {
		fc.MaxImageBytes = cfg.MaxImageBytes
	}
	fc.MaxRedirects = or(cfg.MaxRedirects, d.MaxRedirects)

	return &linkService{
		logger:      logger,
		fetch:       newPreviewer(fc),
		media:       ms,
		st:          st,
		derive:      derive,
		limiter:     linkpreview.NewLimiter(or(cfg.RatePerMinute, d.RatePerMinute), or(cfg.Burst, d.Burst), nil),
		cache:       linkpreview.NewCache[linkCache](linkCacheEntries, nil),
		inflight:    make(chan struct{}, or(cfg.Concurrency, d.Concurrency)),
		ttl:         orD(cfg.CacheTTL, d.CacheTTL),
		negativeTTL: orD(cfg.NegativeCacheTTL, d.NegativeCacheTTL),
		now:         time.Now,
	}
}

// Preview is the api.LinkPreviewFunc.
func (s *linkService) Preview(ctx context.Context, requester uuid.UUID, rawURL string) (api.LinkPreviewResult, error) {
	start := s.now()
	log := func(class string, extra ...any) {
		args := append([]any{"requester_id", requester, "result", class, "duration_ms", s.now().Sub(start).Milliseconds()}, extra...)
		s.logger.InfoContext(ctx, "link preview", args...)
	}

	canon, err := linkpreview.Canonical(rawURL)
	if err != nil {
		log(string(linkpreview.ClassBadURL), "detail", linkpreview.DetailOf(err))
		return api.LinkPreviewResult{BadURL: true}, nil
	}

	if ok, wait := s.limiter.Allow(requester); !ok {
		log("limited", "limit", "account")
		return api.LinkPreviewResult{RetryAfterSec: retryAfter(wait)}, nil
	}

	if hit, ok := s.cache.Get(canon); ok {
		if hit.card == nil {
			log(string(hit.class), "cached", true)
			return api.LinkPreviewResult{}, nil
		}
		res, err := s.mint(ctx, requester, *hit.card)
		if err == nil {
			log("ok", "cached", true)
		}
		return res, err
	}

	select {
	case s.inflight <- struct{}{}:
		defer func() { <-s.inflight }()
	default:
		log("limited", "limit", "server")
		return api.LinkPreviewResult{RetryAfterSec: 1}, nil
	}

	p, err := s.fetch.Preview(ctx, canon)
	if err != nil {
		class := linkpreview.ClassOf(err)
		// A caller that went away says nothing about the page: do not remember it.
		if ctx.Err() == nil {
			s.cache.Put(canon, linkCache{class: class}, s.negativeTTL)
		}
		if class == linkpreview.ClassBlocked {
			// A member pasting an address inside the estate is worth noticing: the
			// address CLASS at warn, never the URL.
			s.logger.WarnContext(ctx, "link preview blocked", "requester_id", requester, "address_class", linkpreview.DetailOf(err))
		}
		log(string(class), "detail", linkpreview.DetailOf(err))
		return api.LinkPreviewResult{}, nil
	}

	card, imageClass, err := s.store(ctx, canon, p)
	if err != nil {
		return api.LinkPreviewResult{}, err
	}
	if card == nil {
		// The page had an image and no title, and the image was not one we could
		// use: nothing is left worth a card.
		s.cache.Put(canon, linkCache{class: linkpreview.ClassEmpty}, s.negativeTTL)
		log(string(linkpreview.ClassEmpty), "image", string(imageClass))
		return api.LinkPreviewResult{}, nil
	}
	s.cache.Put(canon, linkCache{card: card}, s.ttl)
	res, err := s.mint(ctx, requester, *card)
	if err == nil {
		log("ok", "image", card.StorageKey != "", "image_class", string(imageClass))
	}
	return res, err
}

// store ingests the card image, if there is one, and returns the upload row
// (without its uploader). A nil row with a nil error means no card.
//
// An image that cannot be used does not sink a card that has a title: it becomes
// a text-only card, and the reason goes in the log as the returned class.
func (s *linkService) store(ctx context.Context, canon string, p *linkpreview.Preview) (*store.NewUpload, linkpreview.Class, error) {
	row := store.NewUpload{Kind: "link", LinkURL: &canon}
	if p.Title != "" {
		row.LinkTitle = &p.Title
	}
	if p.Description != "" {
		row.LinkDescription = &p.Description
	}
	// og:site_name is not stored (CANT-331 ruling 4 option B): no client shows
	// it, and the host a client shows comes from url and final_host. The fetcher
	// still reads it, as a brand token for the wall rule.
	if p.FinalHost != "" {
		row.LinkFinalHost = &p.FinalHost
	}
	imageClass := p.ImageClass

	if len(p.Image) > 0 {
		img, ierr := media.IngestImage(bytes.NewReader(p.Image))
		switch {
		case ierr == nil:
			key, n, err := s.media.Put(ctx, bytes.NewReader(img.Data), int64(len(img.Data)))
			if err != nil {
				return nil, "", err
			}
			thumbKey, _, err := s.media.Put(ctx, bytes.NewReader(img.Thumb), int64(len(img.Thumb)))
			if err != nil {
				return nil, "", err
			}
			w, h := img.Width, img.Height
			row.StorageKey, row.Bytes = key, n
			row.ThumbKey, row.Width, row.Height, row.Placeholder = &thumbKey, &w, &h, &img.Placeholder
		case errors.Is(ierr, media.ErrUnsupportedType), errors.Is(ierr, media.ErrImageTooLarge):
			imageClass = "image_rejected"
		default:
			return nil, "", ierr
		}
	}
	if row.StorageKey == "" && row.LinkTitle == nil {
		return nil, imageClass, nil
	}
	return &row, imageClass, nil
}

// mint records a fresh handle for requester over a card's row and builds the
// response from the same row, so what the 200 shows is what the send will
// freeze.
func (s *linkService) mint(ctx context.Context, requester uuid.UUID, row store.NewUpload) (api.LinkPreviewResult, error) {
	row.Uploader = requester
	id, err := s.st.CreateUpload(ctx, row)
	if err != nil {
		return api.LinkPreviewResult{}, err
	}
	card := wire.LinkAttachment{
		URL:         *row.LinkURL,
		Title:       row.LinkTitle,
		Description: row.LinkDescription,
		SiteName:    row.LinkSiteName,
		FinalHost:   row.LinkFinalHost,
	}
	if row.StorageKey != "" {
		u := s.derive(row.StorageKey)
		w, h := int64(*row.Width), int64(*row.Height)
		card.ImageURL, card.ImageWidth, card.ImageHeight, card.ImagePlaceholder = &u, &w, &h, row.Placeholder
	}
	return api.LinkPreviewResult{Card: &wire.LinkPreviewResponse{UploadID: id.String(), Preview: card}}, nil
}

// retryAfter rounds a wait up to whole seconds, at least one.
func retryAfter(d time.Duration) int64 {
	return max(1, int64(math.Ceil(d.Seconds())))
}
