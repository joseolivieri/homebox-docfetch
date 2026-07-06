// Package store is the SQLite state layer. It records what docfetch has seen
// and done per Homebox entity so scans stay idempotent, follow-ups are cheap,
// and not-found results back off instead of re-searching every tick.
//
// Pure-Go driver (modernc.org/sqlite) — no cgo, so the release image can be
// minimal/static.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Status values for an entity's doc-fetch lifecycle.
const (
	StatusNew           = "new"
	StatusAttached      = "attached"
	StatusNotFound      = "notfound"
	StatusPendingReview = "pending_review"
	StatusError         = "error"
)

// Record is the per-entity persisted state.
type Record struct {
	EntityID     string
	Name         string
	MetaHash     string // hash of identity fields; change => re-search
	UpdatedAt    string // Homebox updatedAt; change => re-evaluate
	Status       string
	DocSHA256    string
	DocURL       string
	Attempts     int
	FirstSeen    time.Time
	LastChecked  time.Time
	LastAttached *time.Time
}

type Store struct{ db *sql.DB }

// Open opens (creating if needed) the SQLite database and runs migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; avoids SQLITE_BUSY under the scheduler
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS items (
    entity_id     TEXT PRIMARY KEY,
    name          TEXT NOT NULL DEFAULT '',
    meta_hash     TEXT NOT NULL DEFAULT '',
    updated_at    TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'new',
    doc_sha256    TEXT NOT NULL DEFAULT '',
    doc_url       TEXT NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0,
    first_seen    TIMESTAMP NOT NULL,
    last_checked  TIMESTAMP NOT NULL,
    last_attached TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_items_status ON items(status);`)
	return err
}

// MetaHash is the identity fingerprint. When it changes for a known entity, the
// scanner treats the item as changed and re-runs discovery.
func MetaHash(manufacturer, modelNumber, name string) string {
	sum := sha256.Sum256([]byte(manufacturer + "\x00" + modelNumber + "\x00" + name))
	return hex.EncodeToString(sum[:8])
}

// Get returns the record for an entity, or (nil, nil) if absent.
func (s *Store) Get(ctx context.Context, entityID string) (*Record, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT entity_id,name,meta_hash,updated_at,status,doc_sha256,doc_url,attempts,first_seen,last_checked,last_attached
FROM items WHERE entity_id=?`, entityID)
	r, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// Upsert inserts or replaces a record. first_seen is preserved on update.
func (s *Store) Upsert(ctx context.Context, r *Record) error {
	if r.FirstSeen.IsZero() {
		r.FirstSeen = time.Now()
	}
	if r.LastChecked.IsZero() {
		r.LastChecked = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO items (entity_id,name,meta_hash,updated_at,status,doc_sha256,doc_url,attempts,first_seen,last_checked,last_attached)
VALUES (?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(entity_id) DO UPDATE SET
    name=excluded.name, meta_hash=excluded.meta_hash, updated_at=excluded.updated_at,
    status=excluded.status, doc_sha256=excluded.doc_sha256, doc_url=excluded.doc_url,
    attempts=excluded.attempts, last_checked=excluded.last_checked, last_attached=excluded.last_attached`,
		r.EntityID, r.Name, r.MetaHash, r.UpdatedAt, r.Status, r.DocSHA256, r.DocURL,
		r.Attempts, r.FirstSeen, r.LastChecked, r.LastAttached)
	return err
}

// ListByStatus returns all records in the given status.
func (s *Store) ListByStatus(ctx context.Context, status string) ([]*Record, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT entity_id,name,meta_hash,updated_at,status,doc_sha256,doc_url,attempts,first_seen,last_checked,last_attached
FROM items WHERE status=? ORDER BY last_checked`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Record
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DocSHA computes the content hash used for attachment dedupe.
func DocSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type scanner interface{ Scan(...any) error }

func scan(sc scanner) (*Record, error) {
	var r Record
	var lastAttached sql.NullTime
	if err := sc.Scan(&r.EntityID, &r.Name, &r.MetaHash, &r.UpdatedAt, &r.Status,
		&r.DocSHA256, &r.DocURL, &r.Attempts, &r.FirstSeen, &r.LastChecked, &lastAttached); err != nil {
		return nil, fmt.Errorf("scan record: %w", err)
	}
	if lastAttached.Valid {
		r.LastAttached = &lastAttached.Time
	}
	return &r, nil
}
