package works

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"flickr/internal/store"
)

// Cursor is a change-feed position: the library and playback-state change
// counters as of the previous fetch. Sequence numbers, not timestamps —
// clock skew and equal-second writes can't lose changes.
type Cursor struct {
	Lib   int64
	State int64
}

func (c Cursor) String() string { return fmt.Sprintf("l%d.s%d", c.Lib, c.State) }

// ParseCursor parses "l<libSeq>.s<stateSeq>" (e.g. "l1042.s388").
func ParseCursor(s string) (Cursor, error) {
	bad := fmt.Errorf("malformed cursor %q (want l<n>.s<n>)", s)
	rest, ok := strings.CutPrefix(s, "l")
	if !ok {
		return Cursor{}, bad
	}
	libStr, stateStr, ok := strings.Cut(rest, ".s")
	if !ok {
		return Cursor{}, bad
	}
	lib, err1 := strconv.ParseUint(libStr, 10, 63)
	state, err2 := strconv.ParseUint(stateStr, 10, 63)
	if err1 != nil || err2 != nil {
		return Cursor{}, bad
	}
	return Cursor{Lib: int64(lib), State: int64(state)}, nil
}

// Audience is one profile's freshly-derived standing in a feed work.
type Audience struct {
	Name string `json:"name"`
	Progress
}

// FeedWork is one changed work plus every audience with any position in it.
type FeedWork struct {
	Work
	Audiences []Audience `json:"audiences"`
}

// Feed returns the works changed since the cursor: any member item with
// seq > since.Lib, or any playback row on a member with seq > since.State.
// since == nil means everything. A deletionSeq beyond since.Lib also means
// everything — a deleted item leaves no row to compare, so the feed answers
// with a full resync rather than risk a silently stale mirror.
//
// Audiences are recomputed from the given playback rows (all profiles), never
// stored. Works arrive and leave in Build's title order.
func Feed(all []Work, positions []store.Position, since *Cursor, deletionSeq int64) []FeedWork {
	byItem := map[int64][]store.Position{}
	for _, p := range positions {
		byItem[p.ItemID] = append(byItem[p.ItemID], p)
	}

	resync := since == nil || deletionSeq > since.Lib
	var out []FeedWork
	for i := range all {
		w := &all[i]
		changed := resync
		var clients map[string]bool
		for _, it := range w.Items {
			if !changed && it.Seq > since.Lib {
				changed = true
			}
			for _, p := range byItem[it.ID] {
				if !changed && p.Seq > since.State {
					changed = true
				}
				if clients == nil {
					clients = map[string]bool{}
				}
				clients[p.ClientID] = true
			}
		}
		if !changed {
			continue
		}
		fw := FeedWork{Work: *w, Audiences: []Audience{}}
		names := make([]string, 0, len(clients))
		for name := range clients {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			perItem := map[int64]store.Position{}
			for _, it := range w.Items {
				for _, p := range byItem[it.ID] {
					if p.ClientID == name {
						perItem[it.ID] = p
					}
				}
			}
			if pr, ok := WorkProgress(w, perItem); ok {
				fw.Audiences = append(fw.Audiences, Audience{Name: name, Progress: pr})
			}
		}
		out = append(out, fw)
	}
	return out
}
