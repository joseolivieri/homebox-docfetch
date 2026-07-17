package store

import (
	"context"
	"database/sql"
	"time"
)

// Event is one append-only row in the activity log (plan-architecture-v2 M2 /
// D26–D27). Events are written synchronously at decision points — never by
// batch jobs — and replace the entity-notes bus: qr/approve/reject signals are
// events now, and aggregation is read-time SQL over this table.
type Event struct {
	ID         int64
	Ts         time.Time
	EntityID   string
	EntityName string // denormalized for log display without a Homebox call
	Actor      string // portal | scanner | user | system
	Kind       string
	Class      string // manual | parts | photo | warranty | field name | ""
	URL        string
	Detail     string // free-form context (confidence, filenames, cause)
}

// Actors.
const (
	ActorPortal  = "portal"
	ActorScanner = "scanner"
	ActorUser    = "user"
)

// Event kinds. The three signal kinds (EvQRLink, EvDocApprove, EvDocReject)
// are machine-readable state the scanner acts on — they are deduped on write
// and exempt from retention pruning. Everything else is audit history.
const (
	EvIntakeCreated  = "intake.created"
	EvQRLink         = "qr.link"
	EvDocApprove     = "doc.approve"
	EvDocReject      = "doc.reject" // ntfy Reject button OR artifact removed via Homebox (sweep)
	EvDocAttach      = "doc.attach"
	EvDocLink        = "doc.link"
	EvReviewRequest  = "review.request"
	EvPhotoAttach    = "photo.attach"
	EvWarrantySet    = "warranty.set"
	EvEnrichWrite    = "enrich.write"
	EvEnrichOverride = "enrich.override" // user corrected a machine-written value; never refilled
	EvNotFound       = "notfound"        // a class search ended empty this pass (Class says which)
	EvSkimVeto       = "skim.veto"       // content skim rejected a downloaded candidate
	EvError          = "error"
)

// signalKinds are exempt from pruning and deduped per (entity, kind, url).
var signalKinds = map[string]bool{EvQRLink: true, EvDocApprove: true, EvDocReject: true}

func (s *Store) migrateEvents() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY,
    ts          TIMESTAMP NOT NULL,
    entity_id   TEXT NOT NULL,
    entity_name TEXT NOT NULL DEFAULT '',
    actor       TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL,
    class       TEXT NOT NULL DEFAULT '',
    url         TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_events_entity ON events(entity_id, ts);
CREATE INDEX IF NOT EXISTS idx_events_kind ON events(kind, ts);
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);`)
	return err
}

// AppendEvent records an event. Signal kinds (qr/approve/reject) are deduped
// per (entity, kind, url): re-appending an existing signal is a no-op, which
// makes the notes-line importer and repeated button taps idempotent.
func (s *Store) AppendEvent(ctx context.Context, e *Event) error {
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	if signalKinds[e.Kind] {
		var n int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM events WHERE entity_id=? AND kind=? AND url=?`,
			e.EntityID, e.Kind, e.URL).Scan(&n)
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO events (ts, entity_id, entity_name, actor, kind, class, url, detail)
VALUES (?,?,?,?,?,?,?,?)`,
		e.Ts, e.EntityID, e.EntityName, e.Actor, e.Kind, e.Class, e.URL, e.Detail)
	return err
}

// EventURLs returns the distinct URLs of an entity's events of one kind, in
// insertion order. This is the read side of the signal bus (qr / approve /
// reject) that replaced notes-line parsing.
func (s *Store) EventURLs(ctx context.Context, entityID, kind string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT url FROM events WHERE entity_id=? AND kind=? AND url<>'' ORDER BY id`,
		entityID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Events returns recent events, newest first. entityID == "" means all
// entities. Backs the portal /log pages and the `docfetch log` CLI.
func (s *Store) Events(ctx context.Context, entityID string, limit int) ([]*Event, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT id, ts, entity_id, entity_name, actor, kind, class, url, detail
FROM events`
	args := []any{}
	if entityID != "" {
		q += ` WHERE entity_id=?`
		args = append(args, entityID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Ts, &e.EntityID, &e.EntityName, &e.Actor, &e.Kind, &e.Class, &e.URL, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// CountEvents returns how many events an entity has — the "N updates" figure
// in the notes breadcrumb.
func (s *Store) CountEvents(ctx context.Context, entityID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE entity_id=?`, entityID).Scan(&n)
	return n, err
}

// PruneEvents deletes audit events older than the retention window. Signal
// kinds (qr/approve/reject) are permanent state and never pruned (D27).
func (s *Store) PruneEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM events WHERE ts < ? AND kind NOT IN (?,?,?)`,
		olderThan, EvQRLink, EvDocApprove, EvDocReject)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetMeta returns a persisted key's value ("" when absent). Used for the
// change-poll cursor so a container restart doesn't re-prime the signal and
// eat pending change notifications.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetMeta persists a key/value pair.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO meta (key, value) VALUES (?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
