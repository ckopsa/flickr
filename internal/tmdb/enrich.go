package tmdb

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"flickr/internal/model"
	"flickr/internal/store"
)

// libraryStore is the slice of the store the enricher needs (narrow so tests
// can fake it without SQLite).
type libraryStore interface {
	NeedingEnrichment() ([]store.Item, error)
	SetEnrichment(id int64, e *model.Enrichment, ident *model.Identity) error
}

// Enricher fills in TMDB metadata after identification. Failures are silent
// by design: an unenriched item stays unenriched (still NULL in the store)
// and is simply retried on the next scan.
type Enricher struct {
	Client     Client
	Library    libraryStore
	PostersDir string
}

// EnrichAll processes every item needing enrichment, returning how many were
// enriched. Episodes are enriched with their SHOW's entry — one TMDB lookup
// per distinct show per run, cached in memory (posters likewise).
func (e *Enricher) EnrichAll(ctx context.Context) (int, error) {
	items, err := e.Library.NeedingEnrichment()
	if err != nil {
		return 0, err
	}
	shows := map[string]*Result{}  // lowercased show title -> lookup result (nil = known miss)
	posters := map[string][]byte{} // poster path -> bytes, so N episodes = 1 download
	enriched := 0
	for _, it := range items {
		if ctx.Err() != nil {
			return enriched, ctx.Err()
		}
		res, err := e.lookup(ctx, it.Identity, shows)
		if err != nil || res == nil {
			continue // silent: retried next scan
		}
		if e.enrichOne(ctx, it, res, posters) != nil {
			enriched++
		}
	}
	return enriched, nil
}

// EnrichItem enriches a single item on demand (the /enrich endpoint),
// bypassing the "already enriched" filter.
func (e *Enricher) EnrichItem(ctx context.Context, it store.Item) (*model.Enrichment, error) {
	res, err := e.lookup(ctx, it.Identity, map[string]*Result{})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	return e.enrichOne(ctx, it, res, map[string][]byte{}), nil
}

// lookup resolves an identity to a TMDB result: movies by title+year,
// episodes by show title (memoized in shows across a run).
func (e *Enricher) lookup(ctx context.Context, ident *model.Identity, shows map[string]*Result) (*Result, error) {
	if ident == nil {
		return nil, nil
	}
	switch ident.Kind {
	case "movie":
		return e.Client.SearchMovie(ctx, ident.Title, ident.Year)
	case "episode":
		key := strings.ToLower(ident.Title)
		if res, ok := shows[key]; ok {
			return res, nil
		}
		res, err := e.Client.SearchTV(ctx, ident.Title)
		if err != nil {
			return nil, err
		}
		shows[key] = res // cache misses too — don't re-ask for every episode
		return res, nil
	default:
		return nil, nil // unknown items are not enrichable
	}
}

// enrichOne persists one enrichment (poster first, so has_poster is honest)
// and returns it, or nil if persisting failed.
func (e *Enricher) enrichOne(ctx context.Context, it store.Item, res *Result, posters map[string][]byte) *model.Enrichment {
	hasPoster := false
	if res.PosterPath != "" {
		b, ok := posters[res.PosterPath]
		if !ok {
			var err error
			b, err = e.Client.Poster(ctx, res.PosterPath)
			if err != nil {
				b = nil
			}
			posters[res.PosterPath] = b
		}
		if b != nil {
			if err := os.MkdirAll(e.PostersDir, 0o755); err == nil {
				path := filepath.Join(e.PostersDir, strconv.FormatInt(it.ID, 10)+".jpg")
				hasPoster = os.WriteFile(path, b, 0o644) == nil
			}
		}
	}
	enr := mapResult(res, hasPoster)
	if err := e.Library.SetEnrichment(it.ID, enr, it.Identity); err != nil {
		log.Printf("enrich: persist item %d: %v", it.ID, err)
		return nil
	}
	return enr
}

// mapResult is the pure TMDB-result -> enrichment mapping.
func mapResult(r *Result, hasPoster bool) *model.Enrichment {
	return &model.Enrichment{
		TMDBID:    r.ID,
		Title:     r.Title,
		Year:      r.Year,
		Overview:  r.Overview,
		HasPoster: hasPoster,
	}
}
