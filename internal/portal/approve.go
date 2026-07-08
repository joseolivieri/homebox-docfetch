package portal

import (
	"bytes"
	"crypto/hmac"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/notes"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/scheduler"
)

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

// handleApprove is the one-tap target of the ntfy Attach action button:
// verifies the HMAC-signed (entity, url) pair, downloads the candidate doc,
// and attaches it as the manual. Tailnet exposure + signature = no arbitrary
// attach from a crafted link.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	id, docURL := s.verifyAction(w, r, "approve")
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
	for _, a := range detail.Attachments {
		if a.Type == "manual" {
			respondApprove(w, r, detail.Name, "manual already attached")
			return
		}
	}

	data, err := s.eng.Download(ctx, docURL, 50<<20)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("download: %w", err))
		return
	}
	fname := strings.TrimSpace(detail.Manufacturer + "-" + detail.ModelNumber)
	if fname == "-" || fname == "" {
		fname = "manual"
	}
	if _, err := s.hb.UploadAttachment(ctx, id, fname+".pdf", "manual", false, bytes.NewReader(data)); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("attach: %w", err))
		return
	}
	log.Printf("approve: manual attached to %q (%s) via ntfy button", detail.Name, id)
	if fresh, err := s.hb.GetEntity(ctx, id); err == nil {
		upd := fullUpdate(fresh)
		n := notes.Append(fresh.Notes, notes.Line("manual attached via approve "+notes.MDLink("pdf", docURL)))
		upd.Notes = &n
		// Record the source so the item carries provenance like auto-attaches do.
		upd.Fields = homebox.UpsertField(upd.Fields, "Manual", notes.MDLink("pdf", docURL))
		if _, err := s.hb.PutEntity(ctx, id, upd); err != nil {
			log.Printf("approve note put %s: %v", id, err)
		}
	}
	respondApprove(w, r, detail.Name, "manual attached")
}

// handleReject is the one-tap target of the ntfy Reject action button. It
// writes a "rejected [link](url)" line into the entity's docfetch notes block —
// the durable negative label. The scheduler (separate process, separate
// SQLite) ingests it on the next scan: the URL joins the ledger as rejected
// and is never proposed again, and the notes PUT bumps updatedAt so the
// change-poll re-searches within ~30s minus the rejected URL.
func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	id, docURL := s.verifyAction(w, r, "reject")
	if id == "" {
		return
	}
	ctx := r.Context()
	detail, err := s.hb.GetEntity(ctx, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// Idempotent: a second tap is a no-op.
	for _, u := range notes.RejectedURLs(detail.Notes) {
		if u == docURL {
			respondApprove(w, r, detail.Name, "already rejected")
			return
		}
	}
	upd := fullUpdate(detail)
	n := notes.Append(detail.Notes, notes.Line("rejected "+notes.MDLink("link", docURL)))
	upd.Notes = &n
	if _, err := s.hb.PutEntity(ctx, id, upd); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	log.Printf("reject: %q (%s) candidate rejected via ntfy button — %s", detail.Name, id, docURL)
	respondApprove(w, r, detail.Name, "candidate rejected")
}

// respondApprove answers both the ntfy http-action (plain 200) and a browser
// tap (tiny confirmation page).
func respondApprove(w http.ResponseWriter, r *http.Request, name, msg string) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">
<body style="font-family:system-ui;background:#111318;color:#e6e6e9;display:grid;place-items:center;height:100vh;margin:0">
<div style="text-align:center"><div style="font-size:2rem">✓</div><p>%s — %s</p></div></body>`, name, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": msg, "item": name})
}
