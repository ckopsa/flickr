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
	if _, err := db.Exec(metaSchema); err != nil {
		return nil, err
	}
	return &Library{db: db}, nil
}

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
	stmt, err := tx.Prepare(`
		INSERT INTO items (object_key, etag, size, media_info, identity, probe_error, probe_version, identity_version, sidecar_sig, seq, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		if _, err := stmt.Exec(it.ObjectKey, it.ETag, it.Size, mi, id, it.ProbeError, it.ProbeVersion, it.IdentityVersion, it.SidecarSig, seq, now); err != nil {
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

const itemColumns = `id, object_key, etag, size, media_info, identity, identity_overridden, enrichment, probe_error, seq`

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
	if err := rows.Scan(&it.ID, &it.ObjectKey, &it.ETag, &it.Size, &mi, &ident, &overridden, &enr, &it.ProbeError, &it.Seq); err != nil {
		return it, err
	}
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
	// seq: change-feed sequence for playback rows, its own counter in this
	// database's meta table (state.db and library.db never share a tx).
	db.Exec(`ALTER TABLE playback_state ADD COLUMN seq INTEGER NOT NULL DEFAULT 0`)
	// locator: a place in a text (model.Locator as JSON), NULL for every row
	// that is a clock position. Rows from before the column read as NULL,
	// which is correct — nothing had a locator to report then.
	db.Exec(`ALTER TABLE playback_state ADD COLUMN locator TEXT`)
	if _, err := db.Exec(metaSchema); err != nil {
		return nil, err
	}
	return &State{db: db}, nil
}

// ListUsers returns all profile names, oldest first.
func (s *State) ListUsers() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM users ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CreateUser is idempotent: creating an existing profile is a no-op.
func (s *State) CreateUser(name string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO users (name, created_at) VALUES (?, ?)`,
		name, float64(time.Now().UnixMilli())/1000)
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
