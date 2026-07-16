package portal

import (
	"crypto/hmac"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/joseolivieri/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homebox-docfetch/internal/notes"
	"github.com/joseolivieri/homebox-docfetch/internal/sign"
	"github.com/joseolivieri/homebox-docfetch/internal/store"
)

// The ntfy review-gate buttons land here. Both handlers follow the intake
// stage's boundary rule (vision-only remote calls): neither downloads
// anything. They record a doc.approve / doc.reject signal event in the shared
// store (M2/D26) and nudge the scanner to act now. In deprecated split mode
// (legacyNotes) they additionally write the old notes-bus line, which the
// scanner in the other container imports.

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
	want := sign.ActionSig(action, id, docURL, s.cfg.Homebox.Token)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		writeErr(w, http.StatusForbidden, fmt.Errorf("bad signature"))
		return "", ""
	}
	return id, docURL
}

// handleApprove records a one-tap approval (doc.approve event). The scanner
// downloads and attaches the exact approved URL (skipping content
// verification — a human approved it) and records the confirmation label in
// the learning ledger.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	s.queueAction(w, r, "approve", store.EvDocApprove, "approved", "pdf", "manual approved — attaching shortly")
}

// handleReject records a permanent negative label (doc.reject event). The
// scanner never proposes the URL again and re-searches.
func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	s.queueAction(w, r, "reject", store.EvDocReject, "rejected", "link", "candidate rejected")
}

func (s *Server) queueAction(w http.ResponseWriter, r *http.Request, action, kind, keyword, label, okMsg string) {
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
	if kind == store.EvDocApprove {
		for _, a := range detail.Attachments {
			if a.Type == "manual" {
				respondAction(w, r, detail.Name, "manual already attached")
				return
			}
		}
	}
	if existing, err := s.st.EventURLs(ctx, id, kind); err == nil {
		for _, u := range existing {
			if u == docURL {
				respondAction(w, r, detail.Name, "already "+keyword)
				return
			}
		}
	}
	if err := s.st.AppendEvent(ctx, &store.Event{
		EntityID: id, EntityName: detail.Name, Actor: store.ActorUser,
		Kind: kind, URL: docURL, Detail: "ntfy button",
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if s.legacyNotes {
		// Split mode: the scanner cannot see this store — ride the notes bus.
		upd := homebox.FullUpdateFrom(detail)
		n := notes.Append(detail.Notes, notes.Line(keyword+" "+notes.MDLink(label, docURL)))
		upd.Notes = &n
		if _, err := s.hb.PutEntity(ctx, id, upd); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
	}
	if s.trigger != nil {
		s.trigger(id)
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
