package portal

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joseolivieri/homebox-docfetch/internal/store"
)

func TestHandleLogPages(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	_ = st.AppendEvent(ctx, &store.Event{
		EntityID: "e1", EntityName: "Dishwasher", Actor: store.ActorScanner,
		Kind: store.EvDocAttach, Class: "manual", URL: "https://x/m.pdf", Detail: "conf=0.90",
	})
	_ = st.AppendEvent(ctx, &store.Event{
		EntityID: "e2", EntityName: "Toaster <b>", Actor: store.ActorPortal, Kind: store.EvIntakeCreated,
	})
	s := &Server{st: st}

	w := httptest.NewRecorder()
	s.handleLog(w, httptest.NewRequest("GET", "/log", nil))
	body := w.Body.String()
	if !strings.Contains(body, "Dishwasher") || !strings.Contains(body, "doc.attach") {
		t.Fatalf("index missing rows: %s", body)
	}
	if strings.Contains(body, "<b>") {
		t.Fatal("entity name must be HTML-escaped")
	}

	w = httptest.NewRecorder()
	s.handleLog(w, httptest.NewRequest("GET", "/log/e1", nil))
	body = w.Body.String()
	if !strings.Contains(body, "Dishwasher") || strings.Contains(body, "Toaster") {
		t.Fatalf("per-entity filter broken: %s", body)
	}
}
