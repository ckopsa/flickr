// Package works derives the library's self-description: a projection of
// file-level items into WORKS (one movie, one whole show, one audiobook, one
// album, one book), plus per-audience progress over those works and a change
// feed cursored on the store's monotonic sequence numbers.
//
// Everything here is a pure derivation from []store.Item and []store.Position
// — nothing is persisted, nothing knows who consumes the projection.
package works

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"flickr/internal/model"
	"flickr/internal/store"
)

// Work is one presentable unit of the library: a movie, a whole show (its
// episode files grouped), a whole audiobook (its parts grouped), a whole
// album (its tracks grouped), a book, or a bare file we couldn't identify.
type Work struct {
	Key string `json:"work_key"`
	// Kind is the shape of the work: "movie", "show", "audiobook", "album",
	// "book", or "file" for an unidentified item. It names the grouping;
	// Medium names what plays it.
	Kind string `json:"kind"`
	// Medium is "video" (movie, show, and any file work of a video item),
	// "audio" (audiobook, album) or "text" (book) — see model.Medium*.
	Medium string `json:"medium"`
	Title  string `json:"title"`
	// Author is the audiobook's author, the album's artist, the book's
	// author; empty for video works.
	Author string `json:"author,omitempty"`
	Year   int    `json:"year,omitempty"`
	// Genres/Overview come from TMDB enrichment when any member item has it.
	Genres   []string `json:"genres"`
	Overview string   `json:"overview"`
	// EpisodeCount is the number of episode files (0 for movies/files);
	// ExtraCount is the number of bonus-material files attached to the work;
	// PartCount/TrackCount are an audiobook's parts and an album's tracks
	// (0 for every other kind); ItemCount is the total member files.
	EpisodeCount int `json:"episode_count"`
	ExtraCount   int `json:"extra_count"`
	PartCount    int `json:"part_count,omitempty"`
	TrackCount   int `json:"track_count,omitempty"`
	ItemCount    int `json:"item_count"`
	// RepresentativeItemID is the member to fetch artwork for
	// (/api/items/{id}/poster): first member with a poster, else the sorted
	// head — the lowest season/episode, the first part or track, the book.
	RepresentativeItemID int64 `json:"representative_item_id"`

	// Items are the member files: episodes sorted by (season, episode, id),
	// parts and tracks by part number, everything else by id. Not
	// serialized with the work summary.
	Items []store.Item `json:"-"`
}

// Build projects items into works, sorted by title (case-insensitive).
//
// Grouping rules (the same ones the web UI applies client-side today):
//   - kind "movie"  → one work per item; items resolving to the same key
//     (duplicate copies of one film) merge into a single work.
//   - kind "episode" → grouped into one show work by identity title,
//     case-insensitively.
//   - kind "extra" → bonus material, attached to the show (or movie) of the
//     same title as a member that is NOT an episode: it never earns a work,
//     and so never a tile, of its own.
//   - kind "audiobook_part" → one "audiobook" work per (author, title),
//     case-insensitively, its parts ordered by part number.
//   - kind "track" → one "album" work per (artist, album title), likewise.
//   - kind "book" → one "book" work per item (two copies of one book merge
//     on their shared key, exactly as two copies of a film do).
//   - anything else (kind "unknown", missing identity) → a per-item
//     "file:<id>" work, so no file is ever invisible in the projection.
//
// Keys: TMDB id when any member is enriched ("tmdb:<id>" — episodes carry
// the SHOW's TMDB id, so any enriched episode names its show). Fallback is a
// deterministic slug: "<kind>:<lower-title-hyphenated>[-<year>]"; authored
// kinds slug the author in front of the title ("audiobook:frank-herbert-dune")
// so two authors' same-named works stay apart. If a slug key is already
// claimed by a different-kind work (or a TMDB movie/TV id collides
// numerically), the later work falls back to its slug form.
func Build(items []store.Item) []Work {
	var works []Work
	shows := map[string]*Work{} // lower(identity title) → work under construction
	var showOrder []string
	movieAt := map[string]int{} // lower(identity title) → index in works
	var extras []store.Item
	// Audio works under construction, keyed by kind + lower(author) +
	// lower(title); one order list keeps the output deterministic.
	audio := map[string]*Work{}
	var audioOrder []string

	for i := range items {
		it := items[i]
		switch kind(it) {
		case "audiobook_part", "track":
			workKind := "audiobook"
			if kind(it) == "track" {
				workKind = "album"
			}
			k := workKind + "\x00" + strings.ToLower(it.Identity.Author) + "\x00" + strings.ToLower(it.Identity.Title)
			w := audio[k]
			if w == nil {
				w = &Work{Kind: workKind}
				audio[k] = w
				audioOrder = append(audioOrder, k)
			}
			w.Items = append(w.Items, it)
		case "book":
			works = append(works, Work{Kind: "book", Items: []store.Item{it}})
		case "movie":
			works = append(works, Work{Kind: "movie", Items: []store.Item{it}})
			if k := strings.ToLower(it.Identity.Title); k != "" {
				if _, dup := movieAt[k]; !dup {
					movieAt[k] = len(works) - 1
				}
			}
		case "episode":
			k := strings.ToLower(it.Identity.Title)
			w := shows[k]
			if w == nil {
				w = &Work{Kind: "show"}
				shows[k] = w
				showOrder = append(showOrder, k)
			}
			w.Items = append(w.Items, it)
		case "extra":
			extras = append(extras, it) // attached below, once every work exists
		default:
			works = append(works, Work{
				Kind:  "file",
				Key:   fmt.Sprintf("file:%d", it.ID),
				Title: path.Base(it.ObjectKey),
				Items: []store.Item{it},
			})
		}
	}

	// Bonus material joins the work it belongs to — the show first (a show's
	// featurette is far more common than a film's), then a movie of the same
	// title. Extras whose work isn't in the library at all still group
	// together under their title rather than scattering one tile per file.
	for _, it := range extras {
		k := strings.ToLower(it.Identity.Title)
		if w := shows[k]; w != nil {
			w.Items = append(w.Items, it)
			continue
		}
		if i, ok := movieAt[k]; ok {
			works[i].Items = append(works[i].Items, it)
			continue
		}
		w := &Work{Kind: "show", Items: []store.Item{it}}
		shows[k] = w
		showOrder = append(showOrder, k)
	}

	for _, k := range showOrder {
		works = append(works, *shows[k])
	}
	for _, k := range audioOrder {
		works = append(works, *audio[k])
	}

	for i := range works {
		finish(&works[i])
	}
	works = dedupeKeys(works)

	sort.SliceStable(works, func(i, j int) bool {
		a, b := strings.ToLower(works[i].Title), strings.ToLower(works[j].Title)
		if a != b {
			return a < b
		}
		return works[i].Key < works[j].Key
	})
	return works
}

func kind(it store.Item) string {
	if it.Identity == nil {
		return "unknown"
	}
	return it.Identity.Kind
}

func isExtra(it store.Item) bool { return kind(it) == "extra" }

// multiPart reports whether a work is a sequence its audience moves through
// — a show's episodes, an audiobook's parts, an album's tracks — as opposed
// to a single thing (movie, book, stray file). Progress and "up next" treat
// the three alike.
func multiPart(w *Work) bool {
	return w.Kind == "show" || w.Kind == "audiobook" || w.Kind == "album"
}

// mediumFor derives a work's medium from its kind; a file work takes its
// item's own medium, since an unidentified .mp3 is still audio.
func mediumFor(kind string, items []store.Item) string {
	switch kind {
	case "audiobook", "album":
		return model.MediumAudio
	case "book":
		return model.MediumText
	case "file":
		if len(items) > 0 {
			return items[0].MediaInfo.MediumOrVideo()
		}
	}
	return model.MediumVideo
}

// finish sorts a work's members, fills title/year/metadata (enrichment wins
// over identity), computes counts, key, and representative.
func finish(w *Work) {
	sort.SliceStable(w.Items, func(i, j int) bool {
		a, b := w.Items[i], w.Items[j]
		// Bonus material sorts after the work's real content, whatever its
		// numbering: an episode's deleted scenes are not that episode.
		if ax, bx := isExtra(a), isExtra(b); ax != bx {
			return bx
		}
		as, ae := ordinal(a)
		bs, be := ordinal(b)
		if as != bs {
			return as < bs
		}
		if ae != be {
			// Unnumbered members (episode 0, part 0) sit after the numbered
			// ones, ordered by key so a disc-ripped block stays contiguous
			// and in its own order.
			if ae == 0 || be == 0 {
				return be == 0
			}
			return ae < be
		}
		if a.ObjectKey != b.ObjectKey {
			return a.ObjectKey < b.ObjectKey
		}
		return a.ID < b.ID
	})
	w.ItemCount = len(w.Items)
	w.EpisodeCount, w.ExtraCount, w.PartCount, w.TrackCount = 0, 0, 0, 0
	for _, it := range w.Items {
		switch kind(it) {
		case "episode":
			w.EpisodeCount++
		case "extra":
			w.ExtraCount++
		case "audiobook_part":
			w.PartCount++
		case "track":
			w.TrackCount++
		}
	}
	w.Medium = mediumFor(w.Kind, w.Items)

	// Identity-derived title/author/year: from the first member that has them.
	for _, it := range w.Items {
		if it.Identity == nil {
			continue
		}
		if w.Title == "" {
			w.Title = it.Identity.Title
		}
		if w.Author == "" {
			w.Author = it.Identity.Author
		}
		if w.Year == 0 {
			w.Year = it.Identity.Year
		}
		if w.Title != "" && w.Author != "" && w.Year != 0 {
			break
		}
	}
	if w.Title == "" { // file works pre-set this; last resort for the rest
		w.Title = path.Base(w.Items[0].ObjectKey)
	}

	// Enrichment (first enriched member) overrides: for episodes the
	// top-level enrichment fields describe the SHOW, which is exactly what a
	// show work wants.
	var tmdbID int64
	for _, it := range w.Items {
		e := it.Enrichment
		if e == nil || e.TMDBID == 0 {
			continue
		}
		tmdbID = e.TMDBID
		if e.Title != "" {
			w.Title = e.Title
		}
		if e.Year != 0 {
			w.Year = e.Year
		}
		w.Overview = e.Overview
		w.Genres = e.Genres
		break
	}
	if w.Genres == nil {
		w.Genres = []string{}
	}

	if w.Key == "" { // file works arrive keyed
		if tmdbID != 0 {
			w.Key = fmt.Sprintf("tmdb:%d", tmdbID)
		} else {
			w.Key = w.slugKey()
		}
	}

	// Representative: first enriched member with a poster, else the sorted
	// head (lowest season/episode for shows, first part or track, the book,
	// lowest id otherwise).
	w.RepresentativeItemID = w.Items[0].ID
	for _, it := range w.Items {
		if it.Enrichment != nil && it.Enrichment.HasPoster {
			w.RepresentativeItemID = it.ID
			break
		}
	}
}

func seasonEpisode(it store.Item) (int, int) {
	if it.Identity == nil {
		return 0, 0
	}
	return it.Identity.Season, it.Identity.Episode
}

// ordinal is a member's place in its work as a (major, minor) pair: (season,
// episode) for video, (0, part) for audiobook parts and tracks — one sort
// for every multi-part kind.
func ordinal(it store.Item) (int, int) {
	switch kind(it) {
	case "audiobook_part", "track":
		return 0, it.Identity.Part
	}
	return seasonEpisode(it)
}

// slugKey is the work's deterministic fallback key. Authored kinds put the
// author in front of the title, so "Collected Poems" by two poets are two
// works; video kinds keep the historical "<kind>:<title>[-<year>]" form.
func (w *Work) slugKey() string {
	if w.Author != "" {
		return slugKey(w.Kind, w.Author+" "+w.Title, w.Year)
	}
	return slugKey(w.Kind, w.Title, w.Year)
}

// slugKey builds the deterministic fallback key:
// "<kind>:<lower-title-hyphenated>[-<year>]", e.g. "movie:frozen-2013".
func slugKey(kind, title string, year int) string {
	s := slug(title)
	if year != 0 {
		s = fmt.Sprintf("%s-%d", s, year)
	}
	return kind + ":" + s
}

func slug(s string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			hyphen = false
		default:
			if !hyphen && b.Len() > 0 {
				b.WriteByte('-')
				hyphen = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// dedupeKeys guarantees key uniqueness. Same key + same kind = the same
// work (e.g. two copies of one movie): merge members and re-derive. A
// cross-kind collision (a TMDB movie id numerically equal to a show id)
// demotes the later work to its slug key.
func dedupeKeys(works []Work) []Work {
	byKey := map[string]int{}
	var out []Work
	for _, w := range works {
		if i, taken := byKey[w.Key]; taken {
			if out[i].Kind == w.Kind {
				out[i].Items = append(out[i].Items, w.Items...)
				merged := Work{Kind: out[i].Kind, Items: out[i].Items}
				finish(&merged)
				out[i] = merged
				continue
			}
			w.Key = w.slugKey()
			if _, still := byKey[w.Key]; still {
				w.Key = fmt.Sprintf("%s-%d", w.Key, w.RepresentativeItemID)
			}
		}
		byKey[w.Key] = len(out)
		out = append(out, w)
	}
	return out
}

// ByKey indexes works for /api/works/{key}/items lookups.
func ByKey(works []Work) map[string]*Work {
	m := make(map[string]*Work, len(works))
	for i := range works {
		m[works[i].Key] = &works[i]
	}
	return m
}
