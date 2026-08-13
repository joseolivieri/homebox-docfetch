package store

import (
	"context"
	"time"
)

// Fact is one piece of knowledge docfetch holds about an entity that Homebox's
// schema has no home for, or that is a *lead* rather than inventory data
// (plan §6.6). Facts are current state — "what is true" — as distinct from
// events, which are history ("what happened"). Discovery reads facts directly
// instead of replaying the log.
//
// Homebox stays the source of truth for inventory: never mirror a Homebox
// field into a fact, or the two need a sync owner and there isn't one.
type Fact struct {
	EntityID   string
	Kind       string
	Value      string
	Confidence float64
	Source     string
	ObservedAt time.Time
	Superseded bool
}

// Fact kinds. Add one only when a stage will read it — an unread kind is dead
// data (the productType bug, plan §1.4).
const (
	FactGTIN        = "gtin"         // barcode / GS1 AI 01 — canonical product id
	FactFCCID       = "fcc_id"       // FCC equipment authorization id
	FactProductType = "product_type" // "hose timer" — category gate + photo subject
	FactUnitSerial  = "unit_serial"  // GS1 AI 21 / Transparency serial
	FactFieldConf   = "field_conf"   // Value is "<field>=<0..1>": vision read confidence
	FactLeadURL     = "lead_url"     // user- or code-supplied product/support page
	FactSupportURL  = "support_url"  // resolver-supplied support endpoint
)

// Fact sources, strongest first.
const (
	SourceUser     = "user"     // typed or confirmed by a human — definitive
	SourceBarcode  = "barcode"  // deterministic local decode
	SourceQR       = "qr"       // deterministic local decode
	SourceResolver = "resolver" // authoritative external database
	SourceVision   = "vision"   // model read of a photo — carries confidence
	SourceEnrich   = "enrich"   // corroborated web inference
)

func (s *Store) migrateFacts() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS facts (
    entity_id   TEXT NOT NULL,
    kind        TEXT NOT NULL,
    value       TEXT NOT NULL,
    confidence  REAL NOT NULL DEFAULT 0,
    source      TEXT NOT NULL DEFAULT '',
    observed_at TIMESTAMP NOT NULL,
    superseded  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (entity_id, kind, value)
);
CREATE INDEX IF NOT EXISTS idx_facts_entity_kind ON facts(entity_id, kind);`)
	return err
}

// PutFact records a fact, refreshing confidence/source when the same
// (entity, kind, value) is observed again. Idempotent by construction, so
// re-running intake or a scan never duplicates knowledge.
func (s *Store) PutFact(ctx context.Context, f *Fact) error {
	if f.ObservedAt.IsZero() {
		f.ObservedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO facts (entity_id, kind, value, confidence, source, observed_at, superseded)
VALUES (?,?,?,?,?,?,?)
ON CONFLICT(entity_id, kind, value) DO UPDATE SET
    confidence=excluded.confidence, source=excluded.source, observed_at=excluded.observed_at`,
		f.EntityID, f.Kind, f.Value, f.Confidence, f.Source, f.ObservedAt, boolInt(f.Superseded))
	return err
}

// Facts returns an entity's live (non-superseded) facts of one kind, newest
// first. kind == "" returns every kind.
func (s *Store) Facts(ctx context.Context, entityID, kind string) ([]*Fact, error) {
	q := `SELECT entity_id, kind, value, confidence, source, observed_at, superseded
FROM facts WHERE entity_id=? AND superseded=0`
	args := []any{entityID}
	if kind != "" {
		q += ` AND kind=?`
		args = append(args, kind)
	}
	q += ` ORDER BY observed_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Fact
	for rows.Next() {
		var f Fact
		var sup int
		if err := rows.Scan(&f.EntityID, &f.Kind, &f.Value, &f.Confidence, &f.Source, &f.ObservedAt, &sup); err != nil {
			return nil, err
		}
		f.Superseded = sup == 1
		out = append(out, &f)
	}
	return out, rows.Err()
}

// FactValue returns the highest-confidence live value for a kind, or "".
// The common read: "what is this item's GTIN".
func (s *Store) FactValue(ctx context.Context, entityID, kind string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `
SELECT value FROM facts WHERE entity_id=? AND kind=? AND superseded=0
ORDER BY confidence DESC, observed_at DESC LIMIT 1`, entityID, kind).Scan(&v)
	if err != nil {
		return "", nil // no such fact is not an error
	}
	return v, nil
}

// SupersedeFacts marks a kind's facts as superseded — used when a human
// corrects a machine reading. Same discipline as enrichments: a correction is
// final and the old value is never re-asserted.
func (s *Store) SupersedeFacts(ctx context.Context, entityID, kind string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE facts SET superseded=1 WHERE entity_id=? AND kind=?`, entityID, kind)
	return err
}
