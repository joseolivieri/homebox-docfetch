package portal

import (
	"bytes"
	"crypto/hmac"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/notes"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/scheduler"
)

// handleApprove is the one-tap target of the ntfy review-gate action button:
// verifies the HMAC-signed (entity, url) pair, downloads the candidate doc,
// and attaches it as the manual. Tailnet exposure + signature = no arbitrary
// attach from a crafted link.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	docURL := r.URL.Query().Get("url")
	sig := r.URL.Query().Get("sig")
	if id == "" || docURL == "" || sig == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("missing id/url/sig"))
		return
	}
	want := scheduler.ApproveSig(id, docURL, s.cfg.Homebox.Token)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		writeErr(w, http.StatusForbidden, fmt.Errorf("bad signature"))
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
		n := notes.Append(fresh.Notes, notes.Line("manual attached via approve — "+docURL))
		upd.Notes = &n
		if _, err := s.hb.PutEntity(ctx, id, upd); err != nil {
			log.Printf("approve note put %s: %v", id, err)
		}
	}
	respondApprove(w, r, detail.Name, "manual attached")
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
