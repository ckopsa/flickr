// Package store is the storage layer.
//
// Two separate SQLite databases, both in WAL mode:
//   - library.db: mostly-static library metadata (scan writes, playback reads)
//   - state.db:   high-churn playback state (position updates every few seconds)
//
// A library scan hammering library.db can never lock out position writes,
// and vice versa. Scan writes are batched into transactions by the scanner.
package store

import (
	"database/sql"
	"encoding/json"
	"math"
	"time"

	_ "modernc.org/sqlite"

	"flickr/internal/model"
)

func open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // modernc/sqlite: serialize writes through one conn
	return db, nil
}

type Item struct {
	ID                 int64             `json:"id"`
	ObjectKey          string            `json:"object_key"`
	ETag               string            `json:"etag"`
	Size               int64             `json:"size"`
	MediaInfo          *model.MediaInfo  `json:"media_info"`
	Identity           *model.Identity   `json:"identity"`
	IdentityOverridden bool              `json:"identity_overridden"`
	Enrichment         *model.Enrichment `json:"enrichment"`
	ProbeError         string            `json:"probe_error,omitempty"`
	ProbeVersion       int               `json:"-"`
	IdentityVersion    int               `json:"-"`
	// AddedAt is when this file arrived in the library: the object's own
	// LastModified where the scan's listing pass saw one, the moment of the
	// insert otherwise. It is stamped ONCE, on insert — a re-probe or a
	// re-identification is not an arrival, and "recently added" would be a
	// lie if it moved.
	AddedAt time.Time `json:"-"`
	// Seq is the item's change sequence number: every write that touches the
	// row (upsert, identity refresh, enrichment, sidecar re-attach) stamps it
	// from a single monotonic counter (meta.feed_seq). The change feed uses it
	// as a cursor; timestamps are never compared. Internal-only.
	Seq int64 `json:"-"`
	// SidecarSig fingerprints the external-subtitle sidecar set (keys+etags)
	// the media_info was built with; internal-only, the scanner diffs it.
	SidecarSig string `json:"-"`
}

// KnownState is what an incremental scan needs to decide whether to
// re-probe an object (etag + probe recency) or merely re-identify it
// (identity recency, unless the user has pinned the identity) or re-attach
// its subtitle sidecars (sidecar signature).
type KnownState struct {
	ETag               string
	ProbeVersion       int
	IdentityVersion    int
	IdentityOverridden bool
	SidecarSig         string
}

type Library struct{ db *sql.DB }

func OpenLibrary(path string) (*Library, error) {
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS items (
			id INTEGER PRIMARY KEY,
			object_key TEXT NOT NULL UNIQUE,
			etag TEXT NOT NULL,
			size INTEGER NOT NULL,
			media_info TEXT,
			identity TEXT,
			identity_overridden INTEGER NOT NULL DEFAULT 0,
			probe_error TEXT NOT NULL DEFAULT '',
			probe_version INTEGER NOT NULL DEFAULT 0,
			identity_version INTEGER NOT NULL DEFAULT 0,
			enrichment TEXT,
			enrichment_identity TEXT,
			sidecar_sig TEXT NOT NULL DEFAULT '',
			added_at REAL NOT NULL DEFAULT 0,
			updated_at REAL NOT NULL
		)`)
	if err != nil {
		return nil, err
	}
	// Migrations for databases created before these columns existed;
	// the error on an already-present column is expected.
	db.Exec(`ALTER TABLE items ADD COLUMN probe_version INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE items ADD COLUMN identity_version INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE items ADD COLUMN enrichment TEXT`)
	db.Exec(`ALTER TABLE items ADD COLUMN enrichment_identity TEXT`)
	db.Exec(`ALTER TABLE items ADD COLUMN sidecar_sig TEXT NOT NULL DEFAULT ''`)
	// seq: change-feed sequence number. Pre-existing rows keep seq 0, which
	// deliberately sorts before any real cursor — a first feed fetch without
	// `since` sees the whole library, which is the correct initial sync.
	db.Exec(`ALTER TABLE items ADD COLUMN seq INTEGER NOT NULL DEFAULT 0`)
	// added_at: when the file arrived. A row written before the column
	// existed has no arrival time to recover — the bucket's LastModified was
	// never kept — so the whole existing library arrives now, once, at the
	// migration. Only a successful ALTER stamps: on every later open the
	// column is already there and the rows keep what they were given.
	if _, err := db.Exec(`ALTER TABLE items ADD COLUMN added_at REAL NOT NULL DEFAULT 0`); err == nil {
		if _, err := db.Exec(`UPDATE items SET added_at=?`, unixSecs(time.Now())); err != nil {
			return nil, err
		}
	}
	if _, err := db.Exec(metaSchema); err != nil {
		return nil, err
	}
	if _, err := db.Exec(transcriptSchema); err != nil {
		return nil, err
	}
	return &Library{db: db}, nil
}

// transcriptSchema records the generated subtitle track a file with none of
// its own was given (internal/pipeline's whisper stage). The etag is what
// the transcript was generated FROM: a file that changed under its id is
// transcribed again, and one that did not never is.
const transcriptSchema = `
	CREATE TABLE IF NOT EXISTS transcripts (
		item_id INTEGER PRIMARY KEY,
		etag TEXT NOT NULL,
		language TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		generated_at REAL NOT NULL
	)`

// metaSchema holds named monotonic counters. feed_seq is the single change
// counter (bumped once per written row, inside the writing transaction);
// deletion_seq records the counter value at the most recent deletion so the
// feed can demand a full resync instead of tracking per-row tombstones.
const metaSchema = `
	CREATE TABLE IF NOT EXISTS meta (
		key TEXT PRIMARY KEY,
		value INTEGER NOT NULL
	)`

// execQueryer is satisfied by both *sql.DB and *sql.Tx.
type execQueryer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// nextSeq atomically bumps and returns the named counter.
func nextSeq(q execQueryer, key string) (int64, error) {
	var v int64
	err := q.QueryRow(`
		INSERT INTO meta (key, value) VALUES (?, 1)
		ON CONFLICT(key) DO UPDATE SET value = value + 1
		RETURNING value`, key).Scan(&v)
	return v, err
}

func readSeq(q execQueryer, key string) (int64, error) {
	var v int64
	err := q.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}

// unixSecs and timeAt convert between the REAL unix-seconds columns SQLite
// holds (updated_at has always been one) and Go times, at the millisecond
// precision those columns are written with. A zero column is no time at all,
// not 1970.
func unixSecs(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }

func timeAt(secs float64) time.Time {
	if secs == 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(math.Round(secs * 1000))).UTC()
}

// FeedSeq returns the current value of the library change counter — the
// library half of a feed cursor. Every row written after this call will
// carry a strictly greater seq.
func (l *Library) FeedSeq() (int64, error) { return readSeq(l.db, "feed_seq") }

// DeletionSeq returns the counter value stamped by the most recent
// DeleteMissing that actually removed rows (0 if never). A feed cursor older
// than this cannot know which works lost items, so the feed resyncs fully.
func (l *Library) DeletionSeq() (int64, error) { return readSeq(l.db, "deletion_seq") }

// Known returns object_key -> scan-relevant state for incremental scanning.
func (l *Library) Known() (map[string]KnownState, error) {
	rows, err := l.db.Query(`SELECT object_key, etag, probe_version, identity_version, identity_overridden, sidecar_sig FROM items`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]KnownState{}
	for rows.Next() {
		var k string
		var s KnownState
		var overridden int
		if err := rows.Scan(&k, &s.ETag, &s.ProbeVersion, &s.IdentityVersion, &overridden, &s.SidecarSig); err != nil {
			return nil, err
		}
		s.IdentityOverridden = overridden == 1
		out[k] = s
	}
	return out, rows.Err()
}

// UpsertBatch writes a batch of scan results in one transaction.
// Identification is refreshed only if the user hasn't overridden it — a
// corrected match is never silently re-broken by the next scan.
func (l *Library) UpsertBatch(items []Item) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// added_at is deliberately absent from the DO UPDATE list: an upsert on
	// an object we already hold is a re-probe, and a file arrives once.
	stmt, err := tx.Prepare(`
		INSERT INTO items (object_key, etag, size, media_info, identity, probe_error, probe_version, identity_version, sidecar_sig, seq, added_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(object_key) DO UPDATE SET
			etag=excluded.etag,
			size=excluded.size,
			media_info=excluded.media_info,
			probe_error=excluded.probe_error,
			probe_version=excluded.probe_version,
			identity_version=excluded.identity_version,
			sidecar_sig=excluded.sidecar_sig,
			identity=CASE WHEN items.identity_overridden=1
			              THEN items.identity ELSE excluded.identity END,
			seq=excluded.seq,
			updated_at=excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := float64(time.Now().UnixMilli()) / 1000
	for _, it := range items {
		mi, _ := marshalNullable(it.MediaInfo)
		id, _ := marshalNullable(it.Identity)
		seq, err := nextSeq(tx, "feed_seq")
		if err != nil {
			return err
		}
		added := now // no LastModified from the bucket: it arrives now
		if !it.AddedAt.IsZero() {
			added = unixSecs(it.AddedAt)
		}
		if _, err := stmt.Exec(it.ObjectKey, it.ETag, it.Size, mi, id, it.ProbeError, it.ProbeVersion, it.IdentityVersion, it.SidecarSig, seq, added, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// IdentityUpdate is one identity refresh: recomputed identification for an
// unchanged object (no re-probe involved).
type IdentityUpdate struct {
	ObjectKey string
	Identity  model.Identity
}

// UpdateIdentities batch-writes recomputed identities and stamps the new
// identity version. Overridden rows are skipped at the SQL level too, as a
// second line of defense.
func (l *Library) UpdateIdentities(ups []IdentityUpdate, version int) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`
		UPDATE items SET identity=?, identity_version=?, seq=?
		WHERE object_key=? AND identity_overridden=0`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, u := range ups {
		b, err := json.Marshal(u.Identity)
		if err != nil {
			return err
		}
		seq, err := nextSeq(tx, "feed_seq")
		if err != nil {
			return err
		}
		if _, err := stmt.Exec(string(b), version, seq, u.ObjectKey); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SidecarUpdate re-attaches external subtitle tracks to an unchanged item:
// Attach rewrites the stored media_info in place (the scanner owns the
// sidecar-matching logic; the store only owns the read-modify-write).
type SidecarUpdate struct {
	ObjectKey  string
	SidecarSig string
	Attach     func(*model.MediaInfo)
}

// UpdateSidecars applies sidecar re-attachments in one transaction. Rows
// without media_info (probe errors) still get the new signature so the scan
// stops re-flagging them; their subtitles will appear when the probe heals.
func (l *Library) UpdateSidecars(ups []SidecarUpdate) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, u := range ups {
		var mi sql.NullString
		err := tx.QueryRow(`SELECT media_info FROM items WHERE object_key=?`, u.ObjectKey).Scan(&mi)
		if err == sql.ErrNoRows {
			continue // row vanished between listing and now
		}
		if err != nil {
			return err
		}
		seq, err := nextSeq(tx, "feed_seq")
		if err != nil {
			return err
		}
		if !mi.Valid {
			if _, err := tx.Exec(`UPDATE items SET sidecar_sig=?, seq=? WHERE object_key=?`, u.SidecarSig, seq, u.ObjectKey); err != nil {
				return err
			}
			continue
		}
		var info model.MediaInfo
		if err := json.Unmarshal([]byte(mi.String), &info); err != nil {
			return err
		}
		u.Attach(&info)
		b, err := json.Marshal(&info)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE items SET media_info=?, sidecar_sig=?, seq=? WHERE object_key=?`,
			string(b), u.SidecarSig, seq, u.ObjectKey); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func marshalNullable(v any) (any, error) {
	switch x := v.(type) {
	case *model.MediaInfo:
		if x == nil {
			return nil, nil
		}
	case *model.Identity:
		if x == nil {
			return nil, nil
		}
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// DeleteMissing drops items whose object no longer exists (reconciliation pass).
func (l *Library) DeleteMissing(present map[string]bool) (int64, error) {
	rows, err := l.db.Query(`SELECT id, object_key FROM items`)
	if err != nil {
		return 0, err
	}
	var gone []int64
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			rows.Close()
			return 0, err
		}
		if !present[key] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	tx, err := l.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, id := range gone {
		if _, err := tx.Exec(`DELETE FROM items WHERE id=?`, id); err != nil {
			return 0, err
		}
		// The generated transcript belonged to that file, not to the id the
		// next scan may hand to another one.
		if _, err := tx.Exec(`DELETE FROM transcripts WHERE item_id=?`, id); err != nil {
			return 0, err
		}
	}
	// Deletions don't leave a row to carry a seq, and per-row tombstones are
	// more machinery than this feed needs. Instead: stamp the counter value at
	// which the deletion happened; a feed cursor older than deletion_seq gets
	// a full resync (every work), which is always correct, just not minimal.
	if len(gone) > 0 {
		seq, err := nextSeq(tx, "feed_seq")
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`
			INSERT INTO meta (key, value) VALUES ('deletion_seq', ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, seq); err != nil {
			return 0, err
		}
	}
	return int64(len(gone)), tx.Commit()
}

const itemColumns = `id, object_key, etag, size, media_info, identity, identity_overridden, enrichment, probe_error, seq, added_at`

func (l *Library) ListItems() ([]Item, error) {
	rows, err := l.db.Query(`SELECT ` + itemColumns + ` FROM items ORDER BY object_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (l *Library) GetItem(id int64) (*Item, error) {
	rows, err := l.db.Query(`SELECT `+itemColumns+` FROM items WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	it, err := scanItem(rows)
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// Count reports how many items the library holds (startup uses it to decide
// whether an initial scan is warranted).
func (l *Library) Count() (int, error) {
	var n int
	err := l.db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n)
	return n, err
}

func scanItem(rows *sql.Rows) (Item, error) {
	var it Item
	var mi, ident, enr sql.NullString
	var overridden int
	var added float64
	if err := rows.Scan(&it.ID, &it.ObjectKey, &it.ETag, &it.Size, &mi, &ident, &overridden, &enr, &it.ProbeError, &it.Seq, &added); err != nil {
		return it, err
	}
	it.AddedAt = timeAt(added)
	it.IdentityOverridden = overridden == 1
	if mi.Valid {
		it.MediaInfo = &model.MediaInfo{}
		if err := json.Unmarshal([]byte(mi.String), it.MediaInfo); err != nil {
			return it, err
		}
	}
	if ident.Valid {
		it.Identity = &model.Identity{}
		if err := json.Unmarshal([]byte(ident.String), it.Identity); err != nil {
			return it, err
		}
	}
	if enr.Valid {
		it.Enrichment = &model.Enrichment{}
		if err := json.Unmarshal([]byte(enr.String), it.Enrichment); err != nil {
			return it, err
		}
	}
	return it, nil
}

// NeedingEnrichment returns identified items whose enrichment is missing,
// was computed from a different (older) identity, or predates enrichment
// version minVersion (the "v" marker inside the enrichment JSON; rows from
// before the marker existed read as 0). The kind filter (only
// movies/episodes are enrichable) lives in the enricher, which owns that rule.
func (l *Library) NeedingEnrichment(minVersion int) ([]Item, error) {
	rows, err := l.db.Query(`SELECT `+itemColumns+` FROM items
		WHERE identity IS NOT NULL
		  AND (enrichment IS NULL
		       OR enrichment_identity IS NOT identity
		       OR COALESCE(json_extract(enrichment, '$.v'), 0) < ?)
		ORDER BY object_key`, minVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetEnrichment stores an enrichment result along with the identity it was
// derived from, so a later identity change invalidates it automatically.
func (l *Library) SetEnrichment(id int64, e *model.Enrichment, ident *model.Identity) error {
	eb, err := json.Marshal(e)
	if err != nil {
		return err
	}
	ib, err := json.Marshal(ident)
	if err != nil {
		return err
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := nextSeq(tx, "feed_seq")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE items SET enrichment=?, enrichment_identity=?, seq=? WHERE id=?`,
		string(eb), string(ib), seq, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Transcript is the generated subtitle track one item was given: what it is
// in, which model wrote it, when, and the etag of the file it was generated
// from — the transcription stage re-runs only when that etag has moved.
type Transcript struct {
	ItemID      int64     `json:"item_id"`
	ETag        string    `json:"etag"`
	Language    string    `json:"language,omitempty"`
	Model       string    `json:"model,omitempty"`
	GeneratedAt time.Time `json:"generated_at"`
}

// SetTranscript records (or replaces) one item's generated transcript. It is
// not a change to the item, so it bumps no seq: the file the feed describes
// is the same file it was.
func (l *Library) SetTranscript(t Transcript) error {
	_, err := l.db.Exec(`
		INSERT INTO transcripts (item_id, etag, language, model, generated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(item_id) DO UPDATE SET
			etag=excluded.etag, language=excluded.language,
			model=excluded.model, generated_at=excluded.generated_at`,
		t.ItemID, t.ETag, t.Language, t.Model, unixSecs(t.GeneratedAt))
	return err
}

// Transcript returns one item's transcript row, or nil when it has none.
func (l *Library) Transcript(itemID int64) (*Transcript, error) {
	t := Transcript{ItemID: itemID}
	var generated float64
	err := l.db.QueryRow(`SELECT etag, language, model, generated_at FROM transcripts WHERE item_id=?`,
		itemID).Scan(&t.ETag, &t.Language, &t.Model, &generated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.GeneratedAt = timeAt(generated)
	return &t, nil
}

// Transcripts is every transcript row by item id — one query for a stage
// that has to decide about the whole library.
func (l *Library) Transcripts() (map[int64]Transcript, error) {
	rows, err := l.db.Query(`SELECT item_id, etag, language, model, generated_at FROM transcripts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Transcript{}
	for rows.Next() {
		var t Transcript
		var generated float64
		if err := rows.Scan(&t.ItemID, &t.ETag, &t.Language, &t.Model, &generated); err != nil {
			return nil, err
		}
		t.GeneratedAt = timeAt(generated)
		out[t.ItemID] = t
	}
	return out, rows.Err()
}

// OverrideIdentity persists a user correction; scans will never undo it.
// It bumps the item's seq too — an identity change can regroup works, and
// the change feed must see it.
func (l *Library) OverrideIdentity(id int64, ident model.Identity) error {
	b, err := json.Marshal(ident)
	if err != nil {
		return err
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := nextSeq(tx, "feed_seq")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE items SET identity=?, identity_overridden=1, seq=? WHERE id=?`, string(b), seq, id); err != nil {
		return err
	}
	return tx.Commit()
}

type State struct{ db *sql.DB }

func OpenState(path string) (*State, error) {
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS playback_state (
			item_id INTEGER NOT NULL,
			client_id TEXT NOT NULL,
			position_seconds REAL NOT NULL,
			updated_at REAL NOT NULL,
			PRIMARY KEY (item_id, client_id)
		)`)
	if err != nil {
		return nil, err
	}
	// Users are lightweight profiles: progress rows key on client_id, and
	// clients simply pass the profile name as client_id — no join needed.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			name TEXT PRIMARY KEY,
			created_at REAL NOT NULL
		)`)
	if err != nil {
		return nil, err
	}
	// avatar, kid: a profile is a face and an audience, not only a name.
	// Rows from before the columns read as no face and not a kid's, which is
	// exactly what they were.
	db.Exec(`ALTER TABLE users ADD COLUMN avatar TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE users ADD COLUMN kid INTEGER NOT NULL DEFAULT 0`)
	// seq: change-feed sequence for playback rows, its own counter in this
	// database's meta table (state.db and library.db never share a tx).
	db.Exec(`ALTER TABLE playback_state ADD COLUMN seq INTEGER NOT NULL DEFAULT 0`)
	// locator: a place in a text (model.Locator as JSON), NULL for every row
	// that is a clock position. Rows from before the column read as NULL,
	// which is correct — nothing had a locator to report then.
	db.Exec(`ALTER TABLE playback_state ADD COLUMN locator TEXT`)
	// book_display: how a book's pictures are shown on a dark page — a fact
	// about the book (its figures are diagrams, or scans), so one row per
	// item, not per profile. Absent means the format's default.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS book_display (
			item_id INTEGER PRIMARY KEY,
			images TEXT NOT NULL,
			updated_at REAL NOT NULL
		)`); err != nil {
		return nil, err
	}
	// saved: one profile's My List — the things they put by for later. The
	// row keys on a WORK, not an item: a list is a list of titles, and a show
	// whose episodes come and go is one entry either way. It lives beside the
	// playback rows because it is the same kind of state — one profile's, and
	// written while somebody is browsing.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS saved (
			client_id TEXT NOT NULL,
			work_key TEXT NOT NULL,
			saved_at REAL NOT NULL,
			PRIMARY KEY (client_id, work_key)
		)`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(metaSchema); err != nil {
		return nil, err
	}
	return &State{db: db}, nil
}

// Saved is one entry on a profile's list: which work, and when it was put
// there — the order the shelf is read in.
type Saved struct {
	WorkKey string
	SavedAt float64
}

// Save puts one work on a profile's list. Saving a work already on it is a
// no-op that keeps the time it was first put there: a list is a set, and
// pressing the bookmark twice is not a way to shuffle it to the front.
func (s *State) Save(clientID, workKey string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO saved (client_id, work_key, saved_at) VALUES (?, ?, ?)`,
		clientID, workKey, unixSecs(time.Now()))
	return err
}

// Unsave takes it off again, and takes nothing off when it was never on:
// removing what is not there is what a person pressing the tick twice means.
func (s *State) Unsave(clientID, workKey string) error {
	_, err := s.db.Exec(`DELETE FROM saved WHERE client_id=? AND work_key=?`, clientID, workKey)
	return err
}

// SavedFor is one profile's list, newest first. saved_at is a millisecond
// clock, so two things put by in the same instant would otherwise come back
// in whatever order SQLite felt like: the key breaks the tie, and the shelf
// reads the same twice.
func (s *State) SavedFor(clientID string) ([]Saved, error) {
	rows, err := s.db.Query(`SELECT work_key, saved_at FROM saved
		WHERE client_id=? ORDER BY saved_at DESC, work_key`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Saved
	for rows.Next() {
		var sv Saved
		if err := rows.Scan(&sv.WorkKey, &sv.SavedAt); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

// BookDisplay is how a book's pictures are shown on a dark page:
// "themed" (recoloured with the page) or "printed" (left as printed).
// Empty when nobody has chosen, and the format's default applies.
func (s *State) BookDisplay(itemID int64) (string, error) {
	var images string
	err := s.db.QueryRow(`SELECT images FROM book_display WHERE item_id = ?`, itemID).Scan(&images)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return images, err
}

// SetBookDisplay records the choice for one book.
func (s *State) SetBookDisplay(itemID int64, images string) error {
	_, err := s.db.Exec(`INSERT INTO book_display (item_id, images, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(item_id) DO UPDATE SET images = excluded.images, updated_at = excluded.updated_at`,
		itemID, images, float64(time.Now().UnixNano())/1e9)
	return err
}

// User is one profile: the name every playback row is keyed by, the face a
// gate draws it with, and whether it is a child's — the flag the documents
// filter by certification for.
type User struct {
	Name   string `json:"name"`
	Avatar string `json:"avatar,omitempty"`
	Kid    bool   `json:"kid,omitempty"`
}

// ListUsers returns all profiles, oldest first.
func (s *State) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT name, avatar, kid FROM users ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// User is one profile by name, or nil when nobody goes by it.
func (s *State) User(name string) (*User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT name, avatar, kid FROM users WHERE name = ?`, name))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// scanUser reads one row, whichever of the two queries produced it.
func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var kid int
	err := row.Scan(&u.Name, &u.Avatar, &kid)
	u.Kid = kid != 0
	return u, err
}

// CreateUser is idempotent: creating an existing profile is a no-op, and the
// face and audience it already has are the ones it keeps.
func (s *State) CreateUser(u User) error {
	kid := 0
	if u.Kid {
		kid = 1
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO users (name, avatar, kid, created_at) VALUES (?, ?, ?, ?)`,
		u.Name, u.Avatar, kid, float64(time.Now().UnixMilli())/1000)
	return err
}

// SetPosition records a clock position (video, audio): the row's locator is
// cleared, since a place in a text and a place on a clock are one place.
func (s *State) SetPosition(itemID int64, clientID string, pos float64) error {
	return s.SetPlace(itemID, clientID, pos, nil)
}

// SetPlace records where a profile is in an item: a clock position and, for
// text, the locator (stored as JSON in the locator column; NULL when nil).
// One upsert either way — the row is the place, whatever its unit.
func (s *State) SetPlace(itemID int64, clientID string, pos float64, loc *model.Locator) error {
	var locator any
	if loc != nil {
		b, err := json.Marshal(loc)
		if err != nil {
			return err
		}
		locator = string(b)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq, err := nextSeq(tx, "feed_seq")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO playback_state (item_id, client_id, position_seconds, locator, seq, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(item_id, client_id) DO UPDATE SET
			position_seconds=excluded.position_seconds,
			locator=excluded.locator,
			seq=excluded.seq,
			updated_at=excluded.updated_at`,
		itemID, clientID, pos, locator, seq, float64(time.Now().UnixMilli())/1000); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearPlace drops a profile's row for one item: the place forgotten rather
// than moved. It is a DELETE and not a write of zero, because a row at zero
// is still a row — the resume list would have to learn to ignore it, and
// "watched" and "never opened" would look the same.
func (s *State) ClearPlace(itemID int64, clientID string) error {
	_, err := s.db.Exec(`DELETE FROM playback_state WHERE item_id=? AND client_id=?`, itemID, clientID)
	return err
}

// GetPosition returns the profile's row for the item, or the zero Position
// (0 seconds, nil locator) when there is none.
func (s *State) GetPosition(itemID int64, clientID string) (Position, error) {
	ps, err := s.queryPositions(` WHERE item_id=? AND client_id=?`, itemID, clientID)
	if err != nil || len(ps) == 0 {
		return Position{}, err
	}
	return ps[0], nil
}

// Position is one playback-state row: where one profile (client_id) is in
// one item, plus the change seq the feed cursors on. Locator is set only for
// text, where the place is a CFI and a fraction rather than seconds.
type Position struct {
	ItemID          int64
	ClientID        string
	PositionSeconds float64
	Locator         *model.Locator
	UpdatedAt       float64
	Seq             int64
}

func (s *State) queryPositions(where string, args ...any) ([]Position, error) {
	rows, err := s.db.Query(`SELECT item_id, client_id, position_seconds, locator, updated_at, seq FROM playback_state`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Position
	for rows.Next() {
		var p Position
		var loc sql.NullString
		if err := rows.Scan(&p.ItemID, &p.ClientID, &p.PositionSeconds, &loc, &p.UpdatedAt, &p.Seq); err != nil {
			return nil, err
		}
		if loc.Valid && loc.String != "" {
			p.Locator = &model.Locator{}
			if err := json.Unmarshal([]byte(loc.String), p.Locator); err != nil {
				return nil, err
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PositionsFor returns every playback row for one profile.
func (s *State) PositionsFor(clientID string) ([]Position, error) {
	return s.queryPositions(` WHERE client_id=?`, clientID)
}

// AllPositions returns every playback row across all profiles.
func (s *State) AllPositions() ([]Position, error) {
	return s.queryPositions(``)
}

// FeedSeq returns the current value of the playback change counter — the
// state half of a feed cursor.
func (s *State) FeedSeq() (int64, error) { return readSeq(s.db, "feed_seq") }
