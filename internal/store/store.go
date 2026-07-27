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
	ID                 int64            `json:"id"`
	ObjectKey          string           `json:"object_key"`
	ETag               string           `json:"etag"`
	Size               int64            `json:"size"`
	MediaInfo          *model.MediaInfo `json:"media_info"`
	Identity           *model.Identity  `json:"identity"`
	IdentityOverridden bool             `json:"identity_overridden"`
	ProbeError         string           `json:"probe_error,omitempty"`
	ProbeVersion       int              `json:"-"`
}

// KnownState is what an incremental scan needs to decide whether to
// re-probe an object: content identity (etag) and probe recency.
type KnownState struct {
	ETag         string
	ProbeVersion int
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
			updated_at REAL NOT NULL
		)`)
	if err != nil {
		return nil, err
	}
	// Migration for databases created before probe_version existed;
	// the error on an already-present column is expected.
	db.Exec(`ALTER TABLE items ADD COLUMN probe_version INTEGER NOT NULL DEFAULT 0`)
	return &Library{db: db}, nil
}

// Known returns object_key -> (etag, probe_version) for incremental scanning.
func (l *Library) Known() (map[string]KnownState, error) {
	rows, err := l.db.Query(`SELECT object_key, etag, probe_version FROM items`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]KnownState{}
	for rows.Next() {
		var k string
		var s KnownState
		if err := rows.Scan(&k, &s.ETag, &s.ProbeVersion); err != nil {
			return nil, err
		}
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
		INSERT INTO items (object_key, etag, size, media_info, identity, probe_error, probe_version, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(object_key) DO UPDATE SET
			etag=excluded.etag,
			size=excluded.size,
			media_info=excluded.media_info,
			probe_error=excluded.probe_error,
			probe_version=excluded.probe_version,
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
		if _, err := stmt.Exec(it.ObjectKey, it.ETag, it.Size, mi, id, it.ProbeError, it.ProbeVersion, now); err != nil {
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

func (l *Library) ListItems() ([]Item, error) {
	rows, err := l.db.Query(`SELECT id, object_key, etag, size, media_info, identity, identity_overridden, probe_error FROM items ORDER BY object_key`)
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
	rows, err := l.db.Query(`SELECT id, object_key, etag, size, media_info, identity, identity_overridden, probe_error FROM items WHERE id=?`, id)
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

func scanItem(rows *sql.Rows) (Item, error) {
	var it Item
	var mi, ident sql.NullString
	var overridden int
	if err := rows.Scan(&it.ID, &it.ObjectKey, &it.ETag, &it.Size, &mi, &ident, &overridden, &it.ProbeError); err != nil {
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
	return it, nil
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
	return &State{db: db}, nil
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
