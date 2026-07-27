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
	return &Library{db: db}, nil
}

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
		INSERT INTO items (object_key, etag, size, media_info, identity, probe_error, probe_version, identity_version, sidecar_sig, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			updated_at=excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := float64(time.Now().UnixMilli()) / 1000
	for _, it := range items {
		mi, _ := marshalNullable(it.MediaInfo)
		id, _ := marshalNullable(it.Identity)
		if _, err := stmt.Exec(it.ObjectKey, it.ETag, it.Size, mi, id, it.ProbeError, it.ProbeVersion, it.IdentityVersion, it.SidecarSig, now); err != nil {
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
		UPDATE items SET identity=?, identity_version=?
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
		if _, err := stmt.Exec(string(b), version, u.ObjectKey); err != nil {
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
		if !mi.Valid {
			if _, err := tx.Exec(`UPDATE items SET sidecar_sig=? WHERE object_key=?`, u.SidecarSig, u.ObjectKey); err != nil {
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
		if _, err := tx.Exec(`UPDATE items SET media_info=?, sidecar_sig=? WHERE object_key=?`,
			string(b), u.SidecarSig, u.ObjectKey); err != nil {
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
	return int64(len(gone)), tx.Commit()
}

const itemColumns = `id, object_key, etag, size, media_info, identity, identity_overridden, enrichment, probe_error`

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
	if err := rows.Scan(&it.ID, &it.ObjectKey, &it.ETag, &it.Size, &mi, &ident, &overridden, &enr, &it.ProbeError); err != nil {
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
	_, err = l.db.Exec(`UPDATE items SET enrichment=?, enrichment_identity=? WHERE id=?`,
		string(eb), string(ib), id)
	return err
}

// OverrideIdentity persists a user correction; scans will never undo it.
func (l *Library) OverrideIdentity(id int64, ident model.Identity) error {
	b, err := json.Marshal(ident)
	if err != nil {
		return err
	}
	_, err = l.db.Exec(`UPDATE items SET identity=?, identity_overridden=1 WHERE id=?`, string(b), id)
	return err
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

func (s *State) SetPosition(itemID int64, clientID string, pos float64) error {
	_, err := s.db.Exec(`
		INSERT INTO playback_state (item_id, client_id, position_seconds, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(item_id, client_id) DO UPDATE SET
			position_seconds=excluded.position_seconds,
			updated_at=excluded.updated_at`,
		itemID, clientID, pos, float64(time.Now().UnixMilli())/1000)
	return err
}

func (s *State) GetPosition(itemID int64, clientID string) (float64, error) {
	var pos float64
	err := s.db.QueryRow(`SELECT position_seconds FROM playback_state WHERE item_id=? AND client_id=?`,
		itemID, clientID).Scan(&pos)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return pos, err
}
