package portal

import (
	"crypto/hmac"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/joseolivieri/homebox-docfetch/internal/notes"
	"github.com/joseolivieri/homebox-docfetch/internal/scheduler"
)

// The ntfy review-gate buttons land here. Both handlers follow the intake
// stage's boundary rule (vision-only remote calls): neither downloads
// anything. They write a queued "approved"/"rejected" line into the entity's
// docfetch notes block — Homebox is the shared bus between the two stages —
// and the scanner acts on it within ~change_poll seconds (the notes PUT bumps
// updatedAt, which trips the change-poll).

// verifyAction checks the HMAC-signed (action, entity, url) triple common to
// the one-tap ntfy endpoints. Returns ("", "") after writing the error.
func (s *Server) verifyAction(w http.ResponseWriter, r *http.Request, action string) (id, docURL string) {
	id = r.URL.Query().Get("id")
	docURL = r.URL.Query().Get("url")
	sig := r.URL.Query().Get("sig")
	if id == "" || docURL == "" || sig == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("missing id/url/sig"))
		return "", ""
	}
	want := scheduler.ActionSig(action, id, docURL, s.cfg.Homebox.Token)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		writeErr(w, http.StatusForbidden, fmt.Errorf("bad signature"))
		return "", ""
	}
	return id, docURL
}

// handleApprove queues a one-tap approval: "approved [pdf](url)". The scanner
// downloads and attaches the exact approved URL on its next pass (skipping
// content verification — a human approved it) and records the confirmation
// label in the learning ledger.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	s.queueAction(w, r, "approve", "approved", "pdf", "manual approved — attaching shortly")
}

// handleReject queues a permanent negative label: "rejected [link](url)".
// The scanner ingests it, never proposes the URL again, and re-searches.
func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	s.queueAction(w, r, "reject", "rejected", "link", "candidate rejected")
}

func (s *Server) queueAction(w http.ResponseWriter, r *http.Request, action, keyword, label, okMsg string) {
	id, docURL := s.verifyAction(w, r, action)
	if id == "" {
		return
	}
	ctx := r.Context()
	detail, err := s.hb.GetEntity(ctx, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// Idempotent: a second tap (or an already-attached manual) is a no-op.
	existing := notes.RejectedURLs(detail.Notes)
	if keyword == "approved" {
		existing = notes.ApprovedURLs(detail.Notes)
		for _, a := range detail.Attachments {
			if a.Type == "manual" {
				respondAction(w, r, detail.Name, "manual already attached")
				return
			}
		}
	}
	for _, u := range existing {
		if u == docURL {
			respondAction(w, r, detail.Name, "already "+keyword)
			return
		}
	}
	upd := fullUpdate(detail)
	n := notes.Append(detail.Notes, notes.Line(keyword+" "+notes.MDLink(label, docURL)))
	upd.Notes = &n
	if _, err := s.hb.PutEntity(ctx, id, upd); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	log.Printf("%s queued for %q (%s) via ntfy button — %s", keyword, detail.Name, id, docURL)
	respondAction(w, r, detail.Name, okMsg)
}

// respondAction answers both the ntfy http-action (plain 200) and a browser
// tap (tiny confirmation page).
func respondAction(w http.ResponseWriter, r *http.Request, name, msg string) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">
<body style="font-family:system-ui;background:#111318;color:#e6e6e9;display:grid;place-items:center;height:100vh;margin:0">
<div style="text-align:center"><div style="font-size:2rem">✓</div><p>%s — %s</p></div></body>`, name, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": msg, "item": name})
}
