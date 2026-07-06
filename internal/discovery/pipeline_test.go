package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/llm"
)

// fakeSearx serves SearXNG-style JSON and a PDF endpoint for HEAD probes.
func fakeSearx(t *testing.T, results []searxResult) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			http.Error(w, "format not enabled", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(searxResponse{Results: results})
	})
	// endpoints referenced by result URLs, for HEAD content-type/size
	mux.HandleFunc("/manual.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Length", "500000")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(mux)
}

// TestPipelineRulesWinner: a lone model-matching PDF wins with no LLM call.
func TestPipelineRulesWinner(t *testing.T) {
	srv := fakeSearx(t, nil) // set below after we know the base URL
	defer srv.Close()
	// rebuild results using the server's own base URL so HEAD hits our handlers
	results := []searxResult{
		{Title: "Random review", URL: srv.URL + "/blog", Content: "chatter"},
		{Title: "Acme W-1000 Manual", URL: srv.URL + "/manual.pdf", Content: "official Acme W-1000 user guide"},
	}
	// swap the handler's canned results
	srvWithResults(t, srv, results)

	e := NewEngine(Options{
		SearxngURL:    srv.URL,
		Queries:       []string{"{manufacturer} {modelNumber} manual"},
		MinPDFBytes:   1000,
		MaxPDFBytes:   50_000_000,
		RequireModel:  true,
	}, failReranker{t}) // reranker must NOT be called on a clear winner

	res, err := e.Discover(context.Background(), Item{Manufacturer: "Acme", ModelNumber: "W-1000", Name: "Widget"})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if res.UsedLLM {
		t.Fatal("clear winner should not use the LLM")
	}
	if res.Best == nil || !strings.HasSuffix(res.Best.URL, "/manual.pdf") {
		t.Fatalf("expected the PDF to win, got %+v", res.Best)
	}
	if res.Confidence < 0.8 {
		t.Fatalf("expected high confidence, got %v", res.Confidence)
	}
}

// TestPipelineAmbiguousUsesLLM: two model-matching PDFs -> reranker decides.
func TestPipelineAmbiguousUsesLLM(t *testing.T) {
	srv := fakeSearx(t, nil)
	defer srv.Close()
	results := []searxResult{
		{Title: "Acme W-1000 Manual A", URL: srv.URL + "/manual.pdf", Content: "Acme W-1000"},
		{Title: "Acme W-1000 Manual B", URL: srv.URL + "/manual.pdf?v=2", Content: "Acme W-1000 mirror"},
	}
	srvWithResults(t, srv, results)
	// second URL also needs a pdf HEAD; map it in the mux
	// (handled: /manual.pdf pattern won't match query variant, so add explicit)

	e := NewEngine(Options{
		SearxngURL:  srv.URL,
		Queries:     []string{"{modelNumber} manual"},
		MinPDFBytes: 1, MaxPDFBytes: 99_000_000,
	}, stubReranker{idx: 0, conf: 0.85})

	res, err := e.Discover(context.Background(), Item{ModelNumber: "W-1000"})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !res.UsedLLM {
		t.Fatal("ambiguous case should use the LLM")
	}
	if res.Confidence != 0.85 {
		t.Fatalf("expected LLM confidence 0.85, got %v", res.Confidence)
	}
}

// helpers -------------------------------------------------------------

type stubReranker struct {
	idx  int
	conf float64
}

func (s stubReranker) Rerank(_ context.Context, _ string, _ []llm.Candidate) (int, float64, error) {
	return s.idx, s.conf, nil
}

type failReranker struct{ t *testing.T }

func (f failReranker) Rerank(_ context.Context, _ string, _ []llm.Candidate) (int, float64, error) {
	f.t.Fatal("reranker should not have been called")
	return -1, 0, nil
}

// srvWithResults re-registers the /search handler with fresh results.
func srvWithResults(t *testing.T, srv *httptest.Server, results []searxResult) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			http.Error(w, "format not enabled", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(searxResponse{Results: results})
	})
	pdf := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Length", "500000")
	}
	mux.HandleFunc("/manual.pdf", pdf)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/manual.pdf") {
			pdf(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
	})
	srv.Config.Handler = mux
}
