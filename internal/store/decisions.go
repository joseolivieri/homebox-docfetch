package store

import (
	"context"
	"database/sql"
	"time"
)

// Decision is one entry in the learning ledger: everything the doc pipeline
// decided for an entity, with enough context (candidate set, stage, scores) to
// replay or audit it later. Labels arrive after the fact — from the ntfy
// Reject button, from override detection in reconcile, or from long-lived
// attachments counting as confirmations — and turn the ledger into a labeled
// dataset for tuning domain priors, thresholds, and prompts.
type Decision struct {
	ID         int64
	EntityID   string
	EntityName string
	DocClass   string // manual today; quickstart/datasheet/specs later
	Stage      string // pipeline stage that produced the pick ("" = combined)
	ChosenURL  string
	Confidence float64
	UsedLLM    bool
	Candidates string // compact JSON of the scored candidate set
	Outcome    string // attached | linked | review | notfound
	Label      string // "" | confirmed | rejected | overridden
	LabelSrc   string // ntfy | override | age | manual
	CreatedAt  time.Time
	LabeledAt  *time.Time
}

// Label values.
const (
	LabelConfirmed  = "confirmed"
	LabelRejected   = "rejected"
	LabelOverridden = "overridden"
)

func (s *Store) migrateDecisions() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS decisions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_id   TEXT NOT NULL,
    entity_name TEXT NOT NULL DEFAULT '',
    doc_class   TEXT NOT NULL DEFAULT 'manual',
    stage       TEXT NOT NULL DEFAULT '',
    chosen_url  TEXT NOT NULL DEFAULT '',
    confidence  REAL NOT NULL DEFAULT 0,
    used_llm    INTEGER NOT NULL DEFAULT 0,
    candidates  TEXT NOT NULL DEFAULT '',
    outcome     TEXT NOT NULL,
    label       TEXT NOT NULL DEFAULT '',
    label_src   TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMP NOT NULL,
    labeled_at  TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_decisions_entity ON decisions(entity_id, doc_class);
CREATE INDEX IF NOT EXISTS idx_decisions_label ON decisions(label);`)
	return err
}

// RecordDecision appends a ledger row.
func (s *Store) RecordDecision(ctx context.Context, d *Decision) error {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now()
	}
	if d.DocClass == "" {
		d.DocClass = "manual"
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO decisions (entity_id,entity_name,doc_class,stage,chosen_url,confidence,used_llm,candidates,outcome,label,label_src,created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.EntityID, d.EntityName, d.DocClass, d.Stage, d.ChosenURL, d.Confidence,
		boolInt(d.UsedLLM), d.Candidates, d.Outcome, d.Label, d.LabelSrc, d.CreatedAt)
	return err
}

// LabelDecisions applies a label to all unlabeled rows for (entity, url).
// url="" labels every unlabeled row for the entity (used when the user removes
// an attachment and the exact source row is ambiguous). Returns rows labeled.
func (s *Store) LabelDecisions(ctx context.Context, entityID, url, label, source string) (int64, error) {
	q := `UPDATE decisions SET label=?, label_src=?, labeled_at=? WHERE entity_id=? AND label=''`
	args := []any{label, source, time.Now(), entityID}
	if url != "" {
		q += ` AND chosen_url=?`
		args = append(args, url)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RejectedURLs returns every URL rejected for an entity+class — the scanner
// filters these out of future candidate sets.
func (s *Store) RejectedURLs(ctx context.Context, entityID, docClass string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT chosen_url FROM decisions
WHERE entity_id=? AND doc_class=? AND label=? AND chosen_url<>''`,
		entityID, docClass, LabelRejected)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out[u] = true
	}
	return out, rows.Err()
}

// LatestDecision returns the newest ledger row for (entity, class), or nil.
func (s *Store) LatestDecision(ctx context.Context, entityID, docClass string) (*Decision, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id,entity_id,entity_name,doc_class,stage,chosen_url,confidence,used_llm,candidates,outcome,label,label_src,created_at,labeled_at
FROM decisions WHERE entity_id=? AND doc_class=? ORDER BY id DESC LIMIT 1`, entityID, docClass)
	var d Decision
	var usedLLM int
	var labeledAt sql.NullTime
	err := row.Scan(&d.ID, &d.EntityID, &d.EntityName, &d.DocClass, &d.Stage, &d.ChosenURL,
		&d.Confidence, &usedLLM, &d.Candidates, &d.Outcome, &d.Label, &d.LabelSrc, &d.CreatedAt, &labeledAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.UsedLLM = usedLLM != 0
	if labeledAt.Valid {
		d.LabeledAt = &labeledAt.Time
	}
	return &d, nil
}

// DecisionStats aggregates outcome and label counts since a time — the weekly
// digest's accuracy snapshot.
func (s *Store) DecisionStats(ctx context.Context, since time.Time) (outcomes, labels map[string]int, err error) {
	outcomes, labels = map[string]int{}, map[string]int{}
	rows, err := s.db.QueryContext(ctx,
		`SELECT outcome, COUNT(1) FROM decisions WHERE created_at>=? GROUP BY outcome`, since)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, nil, err
		}
		outcomes[k] = n
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	lrows, err := s.db.QueryContext(ctx,
		`SELECT label, COUNT(1) FROM decisions WHERE label<>'' AND labeled_at>=? GROUP BY label`, since)
	if err != nil {
		return nil, nil, err
	}
	defer lrows.Close()
	for lrows.Next() {
		var k string
		var n int
		if err := lrows.Scan(&k, &n); err != nil {
			return nil, nil, err
		}
		labels[k] = n
	}
	return outcomes, labels, lrows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
