// Package portal is the Phase-2 photo-intake web app: snap a model sticker
// and/or receipt on a phone, confirm the extracted fields, and get a fully
// created + enriched + documented Homebox entity. Tailscale-only exposure —
// the tailnet is the access boundary (no auth of its own).
package portal

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/config"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/discovery"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/llm"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/scheduler"
)

//go:embed static
var staticFS embed.FS

const maxUploadBytes = 40 << 20 // up to 4 phone-camera photos

type Server struct {
	cfg *config.Config
	hb  *homebox.Client
	eng *discovery.Engine
	ai  *llm.Client
	sc  *scheduler.Scanner

	unverifiedTagID string
	provenanceTagID string
}

func New(cfg *config.Config, hb *homebox.Client, eng *discovery.Engine, ai *llm.Client, sc *scheduler.Scanner) *Server {
	return &Server{cfg: cfg, hb: hb, eng: eng, ai: ai, sc: sc}
}

// Run bootstraps tags and serves until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	var err error
	if s.unverifiedTagID, err = s.hb.EnsureTag(ctx, s.cfg.Intake.UnverifiedTag); err != nil {
		return fmt.Errorf("ensure unverified tag: %w", err)
	}
	if s.provenanceTagID, err = s.hb.EnsureTag(ctx, s.cfg.Intake.ProvenanceTag); err != nil {
		return fmt.Errorf("ensure provenance tag: %w", err)
	}

	mux := http.NewServeMux()
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/locations", s.handleLocations)
	mux.HandleFunc("/api/extract", s.handleExtract)
	mux.HandleFunc("/api/create", s.handleCreate)
	mux.HandleFunc("/api/approve", s.handleApprove)

	srv := &http.Server{
		Addr:              s.cfg.Portal.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	log.Printf("portal listening on %s", s.cfg.Portal.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	log.Printf("portal error: %v", err)
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// handleLocations lists location entities for the optional dropdown. The flat
// /entities list only returns Item-type entities (verified live), so locations
// come from /entities/tree; nesting is flattened into "Parent › Child" labels.
// No create-location from the portal by design.
func (s *Server) handleLocations(w http.ResponseWriter, r *http.Request) {
	tree, err := s.hb.Tree(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	type loc struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	locs := []loc{}
	var walk func(nodes []homebox.TreeNode, prefix string)
	walk = func(nodes []homebox.TreeNode, prefix string) {
		for _, n := range nodes {
			if n.Type != "location" {
				continue
			}
			label := n.Name
			if prefix != "" {
				label = prefix + " › " + n.Name
			}
			locs = append(locs, loc{ID: n.ID, Name: label})
			walk(n.Children, label)
		}
	}
	walk(tree, "")
	writeJSON(w, http.StatusOK, locs)
}

// handleExtract accepts intake photos (multipart fields sticker/receipt/warranty
// — the personal product photo is not sent here; it carries no extractable data)
// and returns the vision extraction for the confirm screen.
func (s *Server) handleExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var images []llm.IntakeImage
	for _, field := range []string{"sticker", "receipt", "warranty"} {
		if img, ok := formImage(r, field); ok {
			images = append(images, img)
		}
	}
	if len(images) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("no photos submitted"))
		return
	}
	ex, err := s.ai.ExtractIntake(r.Context(), s.cfg.LLM.VisionModel, images)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	// Stateless: the client re-sends the photos with /api/create for attaching.
	writeJSON(w, http.StatusOK, ex)
}

// formImage reads one image field from a parsed multipart form.
func formImage(r *http.Request, field string) (llm.IntakeImage, bool) {
	f, hdr, err := r.FormFile(field)
	if err != nil {
		return llm.IntakeImage{}, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUploadBytes))
	if err != nil || len(data) == 0 {
		return llm.IntakeImage{}, false
	}
	mime := hdr.Header.Get("Content-Type")
	if !strings.HasPrefix(mime, "image/") {
		mime = "image/jpeg"
	}
	return llm.IntakeImage{Data: data, Mime: mime}, true
}
