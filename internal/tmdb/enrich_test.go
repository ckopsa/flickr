package tmdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"flickr/internal/model"
	"flickr/internal/store"
)

type fakeClient struct {
	movieCalls []string
	tvCalls    []string
	results    map[string]*Result // keyed by title
	posterHits int
}

func (f *fakeClient) SearchMovie(_ context.Context, title string, year int) (*Result, error) {
	f.movieCalls = append(f.movieCalls, title)
	return f.results[title], nil
}

func (f *fakeClient) SearchTV(_ context.Context, title string) (*Result, error) {
	f.tvCalls = append(f.tvCalls, title)
	return f.results[title], nil
}

func (f *fakeClient) Poster(_ context.Context, posterPath string) ([]byte, error) {
	f.posterHits++
	return []byte("jpeg-bytes"), nil
}

type fakeLibrary struct {
	items []store.Item
	saved map[int64]*model.Enrichment
}

func (f *fakeLibrary) NeedingEnrichment() ([]store.Item, error) { return f.items, nil }
func (f *fakeLibrary) SetEnrichment(id int64, e *model.Enrichment, _ *model.Identity) error {
	f.saved[id] = e
	return nil
}

func item(id int64, ident model.Identity) store.Item {
	return store.Item{ID: id, Identity: &ident}
}

func TestEnrichAll(t *testing.T) {
	client := &fakeClient{results: map[string]*Result{
		"Frozen":         {ID: 109445, Title: "Frozen", Year: 2013, Overview: "A princess...", PosterPath: "/frozen.jpg"},
		"Rick and Morty": {ID: 60625, Title: "Rick and Morty", Year: 2013, Overview: "A scientist...", PosterPath: "/rm.jpg"},
	}}
	lib := &fakeLibrary{
		items: []store.Item{
			item(1, model.Identity{Kind: "movie", Title: "Frozen", Year: 2013}),
			item(2, model.Identity{Kind: "episode", Title: "Rick and Morty", Season: 1, Episode: 1}),
			item(3, model.Identity{Kind: "episode", Title: "Rick and Morty", Season: 1, Episode: 2}),
			item(4, model.Identity{Kind: "unknown", Title: "beach trip"}),
			item(5, model.Identity{Kind: "movie", Title: "No Such Film", Year: 1999}),
		},
		saved: map[int64]*model.Enrichment{},
	}
	e := &Enricher{Client: client, Library: lib, PostersDir: t.TempDir()}

	n, err := e.EnrichAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("enriched %d items, want 3", n)
	}

	// Movie: mapped per contract, poster written under the item id.
	got := lib.saved[1]
	if got == nil || got.TMDBID != 109445 || got.Title != "Frozen" || got.Year != 2013 || !got.HasPoster {
		t.Errorf("movie enrichment: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(e.PostersDir, "1.jpg")); err != nil {
		t.Errorf("poster file missing: %v", err)
	}

	// Episodes share the SHOW's entry — one TV lookup, one poster download,
	// but each item gets its own poster file.
	if len(client.tvCalls) != 1 {
		t.Errorf("TV lookups = %v, want exactly one for the show", client.tvCalls)
	}
	if client.posterHits != 2 { // one per distinct poster path (movie + show)
		t.Errorf("poster downloads = %d, want 2", client.posterHits)
	}
	for _, id := range []int64{2, 3} {
		if got := lib.saved[id]; got == nil || got.TMDBID != 60625 {
			t.Errorf("episode %d enrichment: %+v", id, got)
		}
		if _, err := os.Stat(filepath.Join(e.PostersDir, fmt.Sprintf("%d.jpg", id))); err != nil {
			t.Errorf("episode %d poster missing: %v", id, err)
		}
	}

	// Unknown kinds and TMDB misses are silently skipped (retried next scan).
	if _, ok := lib.saved[4]; ok {
		t.Error("unknown item was enriched")
	}
	if _, ok := lib.saved[5]; ok {
		t.Error("unmatched item was persisted")
	}
	// Movie search hit the movie endpoint (with the year), not TV.
	if len(client.movieCalls) != 2 { // Frozen + No Such Film
		t.Errorf("movie lookups = %v", client.movieCalls)
	}
}

func TestMapResult(t *testing.T) {
	r := &Result{ID: 603, Title: "The Matrix", Year: 1999, Overview: "There is no spoon."}
	e := mapResult(r, true)
	want := model.Enrichment{TMDBID: 603, Title: "The Matrix", Year: 1999, Overview: "There is no spoon.", HasPoster: true}
	if *e != want {
		t.Errorf("mapResult = %+v, want %+v", *e, want)
	}
}
