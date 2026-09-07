package works

import (
	"sort"
	"strings"
)

// Artist is one artist's shelf: every album work filed under that name, in
// the order the years put them. It is the grid's artist tile and the
// #/artist/<name> view — a projection over Build's output, nothing stored.
type Artist struct {
	Name       string `json:"name"`
	AlbumCount int    `json:"album_count"`
	// Albums in year order, the year-less after the dated, then by title.
	Albums []ArtistAlbum `json:"albums"`
	// RepresentativeItemID is the first album's representative — the item
	// whose /api/items/{id}/cover is the artist's picture.
	RepresentativeItemID int64 `json:"representative_item_id"`
}

// ArtistAlbum is one album as its artist's shelf lists it: enough to draw a
// tile and open the work.
type ArtistAlbum struct {
	Key                  string `json:"work_key"`
	Title                string `json:"title"`
	Year                 int    `json:"year,omitempty"`
	TrackCount           int    `json:"track_count"`
	RepresentativeItemID int64  `json:"representative_item_id"`
}

// Artists groups the album works by artist, case-insensitively, sorted by
// name. The name is spelled the way the artist's first album (in year order)
// spells it. An album with no artist — a loose track in the category
// directory — has no shelf to sit on and is left out; it is still a work on
// /api/works. Audiobooks are authored too but are not music: they never
// appear here.
func Artists(ws []Work) []Artist {
	var albums []Work
	for _, w := range ws {
		if w.Kind == "album" && w.Author != "" {
			albums = append(albums, w)
		}
	}
	// One sort does the whole job: artists come out in name order, each
	// artist's albums in year order behind the first — the one that spells
	// the name.
	sort.SliceStable(albums, func(i, j int) bool {
		x, y := albums[i], albums[j]
		if ax, ay := strings.ToLower(x.Author), strings.ToLower(y.Author); ax != ay {
			return ax < ay
		}
		if x.Year != y.Year {
			if x.Year == 0 || y.Year == 0 {
				return y.Year == 0
			}
			return x.Year < y.Year
		}
		if tx, ty := strings.ToLower(x.Title), strings.ToLower(y.Title); tx != ty {
			return tx < ty
		}
		return x.Key < y.Key
	})

	out := []Artist{}
	for _, w := range albums {
		if n := len(out); n == 0 || !strings.EqualFold(out[n-1].Name, w.Author) {
			out = append(out, Artist{Name: w.Author, RepresentativeItemID: w.RepresentativeItemID})
		}
		a := &out[len(out)-1]
		a.Albums = append(a.Albums, ArtistAlbum{
			Key: w.Key, Title: w.Title, Year: w.Year, TrackCount: w.TrackCount,
			RepresentativeItemID: w.RepresentativeItemID,
		})
		a.AlbumCount = len(a.Albums)
	}
	return out
}
