package works

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"flickr/internal/model"
	"flickr/internal/store"
)

// watchedAt is the fraction of an item's duration past which it counts as
// watched/finished (the resume list drops it, a show episode counts complete).
const watchedAt = 0.9

// minResumeSeconds: positions shorter than this are noise (a misclick, a
// codec probe), not something anyone wants to resume.
const minResumeSeconds = 5

// Finished says whether a playback row has taken its item to the end: past
// watchedAt of the clock, or of the text's own measure (a page over the
// probed count, else the fraction the reader reported). It is the one
// threshold — the resume list drops a finished item, a show counts a
// finished episode, and a work's play action steps over one.
func Finished(it store.Item, p store.Position) bool {
	if p.Locator != nil {
		return textFraction(&it, p.Locator) >= watchedAt
	}
	dur := itemDuration(it)
	return dur > 0 && p.PositionSeconds >= watchedAt*dur
}

// Progress is one audience's standing in one work, derived fresh from
// playback positions — never stored.
type Progress struct {
	Status       string  `json:"status"` // "active" or "finished"
	Fraction     float64 `json:"progress"`
	ProgressText string  `json:"progress_text"`
	UpdatedAt    float64 `json:"updated_at"`
}

// WorkProgress derives one audience's progress in w from that audience's
// positions (item id → position). ok=false when the audience has no
// position on any member item.
//
// Movies (and file works): fraction = position/duration, text "43:12".
// Shows: the FURTHEST (season,episode) with any position anchors the text
// ("S02E05 · 12:30"); the fraction is watched_episodes/episode_count, where
// an episode is watched at ≥90% of its duration, plus the furthest episode's
// partial fraction/episode_count if it isn't itself watched. Finished at
// fraction ≥ 0.9.
// Audiobooks and albums: the furthest part with any position anchors the
// text ("part 3 of 12 · 41:10"); the fraction is time over the SUM of the
// parts' durations, every earlier part counting as heard in full — a book's
// parts are one continuous reading, not episodes to tick off. A single-file
// audiobook reads like a movie. Finished at ≥ 0.9.
// Books: the reader reports a locator — the page's CFI, its section and the
// book's own percentage — and that fraction IS the progress ("ch. 7 · 34%",
// 0.34; see bookProgress). A PDF's locator is a page: "p. 213 / 400" and
// page over the probed page count, or "p. 213" and the reader's own fraction
// when the count is unknown. A row without a locator (written before the
// reader existed) is a position acknowledged but not placed: fraction 0,
// text "", status "active".
func WorkProgress(w *Work, positions map[int64]store.Position) (Progress, bool) {
	var have []store.Position
	var updated float64
	for _, it := range w.Items {
		if p, ok := positions[it.ID]; ok {
			have = append(have, p)
			if p.UpdatedAt > updated {
				updated = p.UpdatedAt
			}
		}
	}
	if len(have) == 0 {
		return Progress{}, false
	}

	if w.Kind == "book" {
		return bookProgress(w, have, updated), true
	}
	if (w.Kind == "audiobook" || w.Kind == "album") && w.PartCount+w.TrackCount > 1 {
		if pr, ok := partsProgress(w, positions, updated); ok {
			return pr, true
		}
	}
	if w.Kind != "show" {
		// Movie/file/single-file audiobook: one relevant item (merged
		// duplicates: the most recent).
		best := have[0]
		for _, p := range have[1:] {
			if p.UpdatedAt > best.UpdatedAt {
				best = p
			}
		}
		dur := durationOf(w, best.ItemID)
		frac := fraction(best.PositionSeconds, dur)
		status := "active"
		if dur > 0 && best.PositionSeconds >= watchedAt*dur {
			status = "finished"
		}
		text := Clock(best.PositionSeconds)
		if w.Medium == model.MediumAudio {
			text = audioClock(itemOf(w, best.ItemID), best.PositionSeconds, dur)
		}
		return Progress{Status: status, Fraction: frac, ProgressText: text, UpdatedAt: updated}, true
	}

	// Show: walk EPISODE members in (season, episode) order. Bonus material is
	// not part of the show's arc — watching every featurette must not report
	// the show as finished — so it anchors no progress here.
	watched := 0
	var furthest *store.Item
	var furthestPos store.Position
	for i := range w.Items {
		it := w.Items[i]
		if isExtra(it) {
			continue
		}
		p, ok := positions[it.ID]
		if !ok {
			continue
		}
		furthest, furthestPos = &w.Items[i], p
		if dur := itemDuration(it); dur > 0 && p.PositionSeconds >= watchedAt*dur {
			watched++
		}
	}
	if furthest == nil || w.EpisodeCount == 0 {
		// Positions exist, but only on bonus material: report the most recent
		// one as a plain in-progress item rather than a place in the show.
		best := have[0]
		for _, p := range have[1:] {
			if p.UpdatedAt > best.UpdatedAt {
				best = p
			}
		}
		dur := durationOf(w, best.ItemID)
		return Progress{
			Status: "active", Fraction: fraction(best.PositionSeconds, dur),
			ProgressText: Clock(best.PositionSeconds), UpdatedAt: updated,
		}, true
	}
	frac := float64(watched) / float64(w.EpisodeCount)
	if dur := itemDuration(*furthest); !(dur > 0 && furthestPos.PositionSeconds >= watchedAt*dur) {
		frac += fraction(furthestPos.PositionSeconds, dur) / float64(w.EpisodeCount)
	}
	status := "active"
	if frac >= watchedAt {
		status = "finished"
	}
	text := Clock(furthestPos.PositionSeconds)
	if s, e := seasonEpisode(*furthest); e > 0 {
		text = fmt.Sprintf("S%02dE%02d · %s", s, e, text)
	}
	return Progress{Status: status, Fraction: frac, ProgressText: text, UpdatedAt: updated}, true
}

// bookProgress is the text derivation. A book has no clock, so the most
// recent row's locator carries the place directly — the book's own
// percentage as the reader measured it, or for a PDF the page — and the text
// names the section (1-based spine order, "ch.") and the percentage: "ch. 7
// · 34%", or "34%" alone when the section is unknown; for a PDF the page
// over the count: "p. 213 / 400", or "p. 213" when the probe found no count.
// Finished at ≥ 0.9 like every other work. Without a locator the position is
// acknowledged but not placed.
func bookProgress(w *Work, have []store.Position, updated float64) Progress {
	best := have[0]
	for _, p := range have[1:] {
		if p.UpdatedAt > best.UpdatedAt {
			best = p
		}
	}
	if best.Locator == nil {
		return Progress{Status: "active", UpdatedAt: updated}
	}
	it := itemOf(w, best.ItemID)
	frac := textFraction(it, best.Locator)
	status := "active"
	if frac >= watchedAt {
		status = "finished"
	}
	return Progress{Status: status, Fraction: frac, ProgressText: bookText(best.Locator, pageCountOf(it)), UpdatedAt: updated}
}

// textFraction is a text place as a fraction of the work: a page over the
// probed page count when the locator is a page and the count is known —
// the server's own arithmetic, so a stale count in the row never wins over
// the library's — else the fraction the reader reported, clamped.
func textFraction(it *store.Item, loc *model.Locator) float64 {
	if loc == nil {
		return 0
	}
	if count := pageCountOf(it); loc.Page > 0 && count > 0 {
		return fraction(float64(loc.Page), float64(count))
	}
	return clamp01(loc.Fraction)
}

// pageCountOf is the item's probed page count; 0 for anything but a PDF
// whose page tree the probe could read.
func pageCountOf(it *store.Item) int {
	if it == nil || it.MediaInfo == nil {
		return 0
	}
	return it.MediaInfo.PageCount
}

// bookText is "p. <page> / <count>" (or "p. <page>" without a count) for a
// paged locator; else "ch. <section> · <pct>%", or "<pct>%" when the section
// is unknown; the percentage is rounded to the nearest whole number.
func bookText(loc *model.Locator, pageCount int) string {
	if loc.Page > 0 {
		if pageCount > 0 {
			return fmt.Sprintf("p. %d / %d", loc.Page, pageCount)
		}
		return fmt.Sprintf("p. %d", loc.Page)
	}
	pct := fmt.Sprintf("%d%%", int(clamp01(loc.Fraction)*100+0.5))
	if loc.Section > 0 {
		return fmt.Sprintf("ch. %d · %s", loc.Section, pct)
	}
	return pct
}

// clamp01 pins a client-reported fraction to [0,1]; NaN reads 0.
func clamp01(f float64) float64 {
	if !(f > 0) {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// partsProgress is the audiobook/album derivation. Members are already in
// part order, so the LAST one with a position is the furthest; everything
// before it counts as heard in full, and the fraction is seconds heard over
// the whole work's seconds. A member with unknown duration adds nothing to
// either side, so the fraction stays honest about what it could measure.
// ok=false when no part carries a position.
func partsProgress(w *Work, positions map[int64]store.Position, updated float64) (Progress, bool) {
	var parts []store.Item
	for _, it := range w.Items {
		if !isExtra(it) {
			parts = append(parts, it)
		}
	}
	furthest := -1
	for i, it := range parts {
		if _, ok := positions[it.ID]; ok {
			furthest = i
		}
	}
	if furthest < 0 {
		return Progress{}, false
	}
	var total, heard float64
	for i, it := range parts {
		dur := itemDuration(it)
		total += dur
		if i < furthest {
			heard += dur
		}
	}
	pos := positions[parts[furthest].ID]
	heard += min(pos.PositionSeconds, itemDuration(parts[furthest]))
	frac := fraction(heard, total)
	status := "active"
	if frac >= watchedAt {
		status = "finished"
	}
	noun := "part"
	if w.Kind == "album" {
		noun = "track"
	}
	text := fmt.Sprintf("%s %d of %d · %s", noun, furthest+1, len(parts), Clock(pos.PositionSeconds))
	return Progress{Status: status, Fraction: frac, ProgressText: text, UpdatedAt: updated}, true
}

// audioClock is the place in a single-file audio work: for a chaptered file
// (an m4b with its chapter atoms) "ch. 7 · 1:19:22 / 11:30:00" — the chapter
// the position falls in, then the position over the whole; an unchaptered
// file reads like a movie, a plain clock.
func audioClock(it *store.Item, pos, dur float64) string {
	text := Clock(pos)
	if it == nil || it.MediaInfo == nil {
		return text
	}
	ch := chapterAt(it.MediaInfo.Chapters, pos)
	if ch == 0 {
		return text
	}
	if dur > 0 {
		text += " / " + Clock(dur)
	}
	return fmt.Sprintf("ch. %d · %s", ch, text)
}

// chapterAt is the 1-based chapter the position falls in — the last chapter
// starting at or before pos; a position ahead of the first marker counts as
// chapter 1. 0 when the file has no chapters.
func chapterAt(chapters []model.Chapter, pos float64) int {
	if len(chapters) == 0 {
		return 0
	}
	n := 1
	for i, c := range chapters {
		if c.StartSeconds <= pos {
			n = i + 1
		}
	}
	return n
}

func itemOf(w *Work, itemID int64) *store.Item {
	for i := range w.Items {
		if w.Items[i].ID == itemID {
			return &w.Items[i]
		}
	}
	return nil
}

func itemDuration(it store.Item) float64 {
	if it.MediaInfo == nil {
		return 0
	}
	return it.MediaInfo.DurationSeconds
}

func durationOf(w *Work, itemID int64) float64 {
	for _, it := range w.Items {
		if it.ID == itemID {
			return itemDuration(it)
		}
	}
	return 0
}

// fraction is position/duration clamped to [0,1]; unknown duration reads 0.
func fraction(pos, dur float64) float64 {
	if dur <= 0 {
		return 0
	}
	f := pos / dur
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// Clock formats seconds as "43:12", or "1:02:03" past the hour. It is the
// way this project spells a time everywhere — a progress line, and the time
// tokens a work offers a passage (the passage grammar reads back exactly
// this spelling).
func Clock(sec float64) string {
	s := int(sec)
	if s < 0 {
		s = 0
	}
	h, m := s/3600, (s%3600)/60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s%60)
	}
	return fmt.Sprintf("%d:%02d", m, s%60)
}

// ContinueEntry is one row of a profile's resume list.
type ContinueEntry struct {
	ItemID          int64   `json:"item_id"`
	WorkKey         string  `json:"work_key"`
	Title           string  `json:"title"` // the work's title
	Label           string  `json:"label"` // "S02E05 · Episode Title", or the file basename
	PositionSeconds float64 `json:"position_seconds"`
	DurationSeconds float64 `json:"duration_seconds"`
	// Fraction is a text entry's place as the book's own percentage — the
	// seconds/duration pair says nothing about a book. 0 (omitted) for
	// everything with a clock.
	Fraction  float64 `json:"fraction,omitempty"`
	UpdatedAt float64 `json:"updated_at"`
}

// ContinueList derives one profile's resume list: most-recent first, at most
// max entries, excluding sub-5-second positions and collapsing each work to
// its single most-recent item. A finished item (≥90% of duration) normally
// drops out — but a finished show episode with a following episode advances
// the entry to that NEXT episode ("up next"): position 0 unless the profile
// already has its own unfinished progress there, which wins. An audiobook's
// parts and an album's tracks advance the same way. A finished finale (no
// next member) and a finished movie are excluded as before. A book is
// resumable by its locator (see resumable) and finished at fraction ≥ 0.9;
// its entry carries that fraction.
func ContinueList(works []Work, positions []store.Position, max int) []ContinueEntry {
	itemWork := map[int64]*Work{}
	items := map[int64]*store.Item{}
	for i := range works {
		w := &works[i]
		for j := range w.Items {
			itemWork[w.Items[j].ID] = w
			items[w.Items[j].ID] = &w.Items[j]
		}
	}

	// Most-recent position per work (finished or not — finishing an episode
	// IS the profile's latest activity in the show), and per item (so an
	// advanced entry can pick up the next episode's own progress).
	latest := map[string]store.Position{} // work key → most recent position
	byItem := map[int64]store.Position{}  // item id → most recent position
	for _, p := range positions {
		if !resumable(p) {
			continue // noise (a misclick, a codec probe, a glance at a cover)
		}
		w := itemWork[p.ItemID]
		if w == nil {
			continue // stale position for a deleted item
		}
		if prev, ok := byItem[p.ItemID]; !ok || p.UpdatedAt > prev.UpdatedAt {
			byItem[p.ItemID] = p
		}
		if prev, ok := latest[w.Key]; ok && prev.UpdatedAt >= p.UpdatedAt {
			continue
		}
		latest[w.Key] = p
	}

	out := make([]ContinueEntry, 0, len(latest))
	for key, p := range latest {
		w, it := itemWork[p.ItemID], items[p.ItemID]
		dur := itemDuration(*it)
		if p.Locator != nil {
			// A text place: the fraction is the whole story (a page over the
			// count for a PDF). A finished book has nothing to advance to.
			frac := textFraction(it, p.Locator)
			if frac >= watchedAt {
				continue
			}
			out = append(out, ContinueEntry{
				ItemID: it.ID, WorkKey: key, Title: w.Title, Label: ItemLabel(*it),
				DurationSeconds: dur, Fraction: frac, UpdatedAt: p.UpdatedAt,
			})
			continue
		}
		if dur > 0 && p.PositionSeconds >= watchedAt*dur { // finished
			if !multiPart(w) {
				continue // finished movie/book/file: nothing to resume
			}
			next := nextMember(w, p.ItemID)
			if next == nil {
				continue // finished the finale (or last part): the work is done
			}
			// Advance to the next member. Its own unfinished progress wins;
			// otherwise it starts at 0. The finished watch remains the entry's
			// recency (UpdatedAt) — it IS the latest activity.
			it, dur = next, itemDuration(*next)
			p.PositionSeconds = 0
			if np, ok := byItem[next.ID]; ok && !(dur > 0 && np.PositionSeconds >= watchedAt*dur) {
				p.PositionSeconds = np.PositionSeconds
			}
		}
		out = append(out, ContinueEntry{
			ItemID:          it.ID,
			WorkKey:         key,
			Title:           w.Title,
			Label:           ItemLabel(*it),
			PositionSeconds: p.PositionSeconds,
			DurationSeconds: dur,
			UpdatedAt:       p.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].ItemID < out[j].ItemID
	})
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// resumable says whether a playback row is a place anyone wants back: a
// clock position of at least minResumeSeconds, or a text locator past the
// opening — any fraction at all, or any section or page after the first (the
// cover, the title page). Opening a book and closing it on its cover is the
// text version of a three-second misclick.
func resumable(p store.Position) bool {
	if p.Locator != nil {
		return p.Locator.Fraction > 0 || p.Locator.Section > 1 || p.Locator.Page > 1
	}
	return p.PositionSeconds >= minResumeSeconds
}

// Neighbours are the members either side of itemID in the work's own
// ordering — the previous and next episode by (season, episode), the
// previous and next part or track by number; w.Items is already sorted that
// way, with bonus material last. Bonus material is skipped on both sides
// (finishing an episode never advances into a featurette) and an item that
// is itself bonus material — or not a member at all — has no neighbours: it
// sits outside the order the work is moved through.
func Neighbours(w *Work, itemID int64) (prev, next *store.Item) {
	for i := range w.Items {
		if w.Items[i].ID != itemID || isExtra(w.Items[i]) {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			if !isExtra(w.Items[j]) {
				prev = &w.Items[j]
				break
			}
		}
		for j := i + 1; j < len(w.Items); j++ {
			if !isExtra(w.Items[j]) {
				next = &w.Items[j]
				break
			}
		}
		return prev, next
	}
	return nil, nil
}

// nextMember is the next real member after itemID, or nil at the finale.
func nextMember(w *Work, itemID int64) *store.Item {
	_, next := Neighbours(w, itemID)
	return next
}

// ItemLabel names one item inside its work: numbered episodes get
// "S02E05 · <title>" when enrichment knows the episode's title, and the code
// alone when nobody does; a numbered audiobook part is "Part 3"; a track is
// "Track 3 · <its title>" when the path named it and "Track 3" when the
// identity predates track titles. Everything else — bonus material, episodes
// and parts nobody numbered, films, books — is ItemTitle, since a made-up
// "S00E00" or "Part 0" names nothing.
func ItemLabel(it store.Item) string {
	id := it.Identity
	if id == nil {
		return ItemTitle(it)
	}
	switch id.Kind {
	case "episode":
		if id.Episode > 0 {
			code := fmt.Sprintf("S%02dE%02d", id.Season, id.Episode)
			if it.Enrichment != nil && it.Enrichment.EpisodeTitle != "" {
				return code + " · " + it.Enrichment.EpisodeTitle
			}
			return code
		}
	case "audiobook_part":
		if id.Part > 0 {
			return fmt.Sprintf("Part %d", id.Part)
		}
	case "track":
		if id.Part > 0 {
			if id.TrackTitle != "" {
				return fmt.Sprintf("Track %d · %s", id.Part, id.TrackTitle)
			}
			return fmt.Sprintf("Track %d", id.Part)
		}
	}
	return ItemTitle(it)
}

// ItemTitle is what a screen calls this one file, and it is never the file's
// name: the work's own title where the work IS this one file (a film, a
// book, an audiobook nobody split), the episode's title from TMDB — else
// "Episode 5" — for an episode, the track's own name for a track, and
// "Part 3" for an audiobook part, or the chapter a part names itself after.
// Bonus material, and anything the path said nothing about, falls back to
// the file's STEM tidied into words: a name a person typed is still a name,
// but ".mkv" was never part of it.
func ItemTitle(it store.Item) string {
	id, e := it.Identity, it.Enrichment
	if id == nil {
		return stemTitle(it.ObjectKey)
	}
	switch id.Kind {
	case "episode":
		if e != nil && e.EpisodeTitle != "" {
			return e.EpisodeTitle
		}
		if id.Episode > 0 {
			return fmt.Sprintf("Episode %d", id.Episode)
		}
	case "audiobook_part":
		if id.Part > 0 {
			if ch := soleChapterTitle(it); ch != "" {
				return ch
			}
			return fmt.Sprintf("Part %d", id.Part)
		}
		return workTitle(it) // the whole book in one file
	case "track":
		if id.TrackTitle != "" {
			return id.TrackTitle
		}
		if id.Part > 0 {
			return fmt.Sprintf("Track %d", id.Part)
		}
		return workTitle(it)
	case "movie", "book":
		return workTitle(it)
	}
	return stemTitle(it.ObjectKey)
}

// workTitle is the title borne by the WORK this file belongs to — TMDB's
// where the enrichment found one, the identification's otherwise.
func workTitle(it store.Item) string {
	if e := it.Enrichment; e != nil && e.Title != "" {
		return e.Title
	}
	if it.Identity != nil && it.Identity.Title != "" {
		return it.Identity.Title
	}
	return stemTitle(it.ObjectKey)
}

// soleChapterTitle is the name a one-chapter file gives itself: an audiobook
// cut one chapter to a file says which chapter this is better than its part
// number does. Two chapters or more and the file is not one chapter.
func soleChapterTitle(it store.Item) string {
	if mi := it.MediaInfo; mi != nil && len(mi.Chapters) == 1 {
		return strings.TrimSpace(mi.Chapters[0].Title)
	}
	return ""
}

// stemTitle reads an object key the way a person would: the base name with
// its extension dropped and the underscores and dots that stand in for
// spaces put back. It is the last resort, not the rule — and a stem that
// tidies away to nothing keeps the base name it came from.
func stemTitle(key string) string {
	name := path.Base(key)
	if ext := path.Ext(name); ext != "" && ext != name {
		name = name[:len(name)-len(ext)]
	}
	name = strings.Join(strings.Fields(strings.NewReplacer("_", " ", ".", " ").Replace(name)), " ")
	if name == "" {
		return path.Base(key)
	}
	return name
}
