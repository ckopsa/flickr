package works

import (
	"fmt"
	"path"
	"sort"

	"flickr/internal/store"
)

// watchedAt is the fraction of an item's duration past which it counts as
// watched/finished (the resume list drops it, a show episode counts complete).
const watchedAt = 0.9

// minResumeSeconds: positions shorter than this are noise (a misclick, a
// codec probe), not something anyone wants to resume.
const minResumeSeconds = 5

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

	if w.Kind != "show" {
		// Movie/file: single relevant item (merged duplicates: most recent).
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
		return Progress{Status: status, Fraction: frac, ProgressText: clock(best.PositionSeconds), UpdatedAt: updated}, true
	}

	// Show: walk members in (season, episode) order.
	watched := 0
	var furthest *store.Item
	var furthestPos store.Position
	for i := range w.Items {
		it := w.Items[i]
		p, ok := positions[it.ID]
		if !ok {
			continue
		}
		furthest, furthestPos = &w.Items[i], p
		if dur := itemDuration(it); dur > 0 && p.PositionSeconds >= watchedAt*dur {
			watched++
		}
	}
	frac := float64(watched) / float64(w.EpisodeCount)
	if dur := itemDuration(*furthest); !(dur > 0 && furthestPos.PositionSeconds >= watchedAt*dur) {
		frac += fraction(furthestPos.PositionSeconds, dur) / float64(w.EpisodeCount)
	}
	status := "active"
	if frac >= watchedAt {
		status = "finished"
	}
	s, e := seasonEpisode(*furthest)
	text := fmt.Sprintf("S%02dE%02d · %s", s, e, clock(furthestPos.PositionSeconds))
	return Progress{Status: status, Fraction: frac, ProgressText: text, UpdatedAt: updated}, true
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

// clock formats seconds as "43:12", or "1:02:03" past the hour.
func clock(sec float64) string {
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
	UpdatedAt       float64 `json:"updated_at"`
}

// ContinueList derives one profile's resume list: most-recent first, at most
// max entries, excluding sub-5-second positions and collapsing each work to
// its single most-recent item. A finished item (≥90% of duration) normally
// drops out — but a finished show episode with a following episode advances
// the entry to that NEXT episode ("up next"): position 0 unless the profile
// already has its own unfinished progress there, which wins. A finished
// finale (no next episode) and a finished movie are excluded as before.
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
		if p.PositionSeconds < minResumeSeconds {
			continue // noise (a misclick, a codec probe)
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
		if dur > 0 && p.PositionSeconds >= watchedAt*dur { // finished
			if w.Kind != "show" {
				continue // finished movie/file: nothing to resume
			}
			next := nextEpisode(w, p.ItemID)
			if next == nil {
				continue // finished the finale: the show is done
			}
			// Advance to the next episode. Its own unfinished progress wins;
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
			Label:           itemLabel(*it),
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

// nextEpisode is the member after itemID in the show's (season, episode)
// ordering — w.Items is already sorted that way — or nil at the finale.
func nextEpisode(w *Work, itemID int64) *store.Item {
	for i := range w.Items {
		if w.Items[i].ID == itemID && i+1 < len(w.Items) {
			return &w.Items[i+1]
		}
	}
	return nil
}

// itemLabel names one item inside its work: episodes get "S02E05 · <title>"
// (enrichment episode title when known, else the file basename); everything
// else is just the basename.
func itemLabel(it store.Item) string {
	name := path.Base(it.ObjectKey)
	if it.Identity == nil || it.Identity.Kind != "episode" {
		return name
	}
	if it.Enrichment != nil && it.Enrichment.EpisodeTitle != "" {
		name = it.Enrichment.EpisodeTitle
	}
	return fmt.Sprintf("S%02dE%02d · %s", it.Identity.Season, it.Identity.Episode, name)
}
