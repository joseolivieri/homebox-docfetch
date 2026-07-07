package store

import (
	"context"
	"time"
)

// Enrichment is one audited machine-write of a metadata field.
type Enrichment struct {
	EntityID     string
	Field        string // manufacturer | modelNumber | name | category
	Value        string
	Confidence   float64
	EvidenceURLs string // comma-joined
	WrittenAt    time.Time
	Superseded   bool // set when a human edit replaces the value
}

func (s *Store) migrateEnrich() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS enrichments (
    entity_id     TEXT NOT NULL,
    field         TEXT NOT NULL,
    value         TEXT NOT NULL,
    confidence    REAL NOT NULL,
    evidence_urls TEXT NOT NULL DEFAULT '',
    written_at    TIMESTAMP NOT NULL,
    superseded    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (entity_id, field)
);`)
	return err
}

// RecordEnrichment inserts or replaces the audit row for (entity, field).
func (s *Store) RecordEnrichment(ctx context.Context, e *Enrichment) error {
	if e.WrittenAt.IsZero() {
		e.WrittenAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO enrichments (entity_id,field,value,confidence,evidence_urls,written_at,superseded)
VALUES (?,?,?,?,?,?,?)
ON CONFLICT(entity_id,field) DO UPDATE SET
    value=excluded.value, confidence=excluded.confidence,
    evidence_urls=excluded.evidence_urls, written_at=excluded.written_at,
    superseded=excluded.superseded`,
		e.EntityID, e.Field, e.Value, e.Confidence, e.EvidenceURLs, e.WrittenAt, e.Superseded)
	return err
}

// Enrichments returns all audit rows for an entity (empty slice if none).
func (s *Store) Enrichments(ctx context.Context, entityID string) ([]*Enrichment, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT entity_id,field,value,confidence,evidence_urls,written_at,superseded
FROM enrichments WHERE entity_id=?`, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Enrichment
	for rows.Next() {
		var e Enrichment
		if err := rows.Scan(&e.EntityID, &e.Field, &e.Value, &e.Confidence,
			&e.EvidenceURLs, &e.WrittenAt, &e.Superseded); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// AlreadyEnriched reports whether (entity, field) has a non-superseded audit row.
func (s *Store) AlreadyEnriched(ctx context.Context, entityID, field string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM enrichments WHERE entity_id=? AND field=? AND superseded=0`,
		entityID, field).Scan(&n)
	return n > 0, err
}
