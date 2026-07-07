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

const maxUploadBytes = 20 << 20 // 2 photos, phone-camera sized

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

// handleLocations lists location entities (entityType.isLocation) for the
// optional dropdown. No create-location from the portal by design.
func (s *Server) handleLocations(w http.ResponseWriter, r *http.Request) {
	list, err := s.hb.ListEntities(r.Context(), 1, 200, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	type loc struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	locs := []loc{}
	for _, e := range list.Items {
		if e.EntityType.IsLocation {
			locs = append(locs, loc{ID: e.ID, Name: e.Name})
		}
	}
	writeJSON(w, http.StatusOK, locs)
}

// handleExtract accepts 1-2 photos (multipart fields photo1/photo2) and returns
// the vision extraction for the confirm screen.
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
	for _, field := range []string{"photo1", "photo2"} {
		f, hdr, err := r.FormFile(field)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxUploadBytes))
		f.Close()
		if err != nil || len(data) == 0 {
			continue
		}
		mime := hdr.Header.Get("Content-Type")
		if !strings.HasPrefix(mime, "image/") {
			mime = "image/jpeg"
		}
		images = append(images, llm.IntakeImage{Data: data, Mime: mime})
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

	// Store the receipt image server-side for attach-on-create? Keep stateless:
	// the client re-sends the receipt photo with /api/create instead.
	writeJSON(w, http.StatusOK, ex)
}
