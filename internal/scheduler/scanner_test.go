package scheduler

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/discovery"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/notify"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/store"
)

// --- fakes ---

type fakeAPI struct {
	list     []homebox.EntitySummary
	details  map[string]*homebox.EntityOut
	uploads  int
	patches  int
	puts     int
	lastTags []string
}

func (f *fakeAPI) ListEntities(_ context.Context, page, pageSize int, tagIDs []string) (*homebox.EntityListResult, error) {
	if page > 1 {
		return &homebox.EntityListResult{Page: page, PageSize: pageSize, Total: len(f.list)}, nil
	}
	return &homebox.EntityListResult{Page: 1, PageSize: pageSize, Total: len(f.list), Items: f.list}, nil
}
func (f *fakeAPI) GetEntity(_ context.Context, id string) (*homebox.EntityOut, error) {
	return f.details[id], nil
}
func (f *fakeAPI) PatchEntity(_ context.Context, id string, in homebox.EntityUpdate) (*homebox.EntityOut, error) {
	f.patches++
	f.lastTags = in.TagIDs
	return f.details[id], nil
}
func (f *fakeAPI) PutEntity(_ context.Context, id string, in homebox.EntityUpdate) (*homebox.EntityOut, error) {
	f.puts++
	f.lastTags = in.TagIDs
	d := f.details[id]
	if in.Manufacturer != nil {
		d.Manufacturer = *in.Manufacturer
	}
	if in.ModelNumber != nil {
		d.ModelNumber = *in.ModelNumber
	}
	return d, nil
}
func (f *fakeAPI) UploadAttachment(_ context.Context, id, name, t string, p bool, r io.Reader) (*homebox.EntityOut, error) {
	f.uploads++
	return f.details[id], nil
}
func (f *fakeAPI) EnsureTag(_ context.Context, name string) (string, error) {
	return "tag-unverified", nil
}

type fakeDisc struct {
	res       *discovery.Result
	body      []byte
	discCalls int
}

func (d *fakeDisc) Discover(_ context.Context, _ discovery.Item) (*discovery.Result, error) {
	d.discCalls++
	return d.res, nil
}
func (d *fakeDisc) Download(_ context.Context, _ string, _ int64) ([]byte, error) { return d.body, nil }
func (d *fakeDisc) VerifyPDF(_ context.Context, _ discovery.Item, _ []byte) bool  { return true }

type fakeNtfy struct{ sent int }

func (n *fakeNtfy) Send(_ context.Context, _ notify.Message) error { n.sent++; return nil }

// --- helpers ---

func newTestScanner(t *testing.T, api *fakeAPI, disc *fakeDisc, nt *fakeNtfy) (*Scanner, *store.Store) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	sc := NewScanner(api, disc, nt, st, Config{
		AutoAttachThreshold: 0.7,
		SkipIfManualExists:  true,
		MaxPDFBytes:         10_000_000,
		UnverifiedTag:       "docfetch/unverified",
	})
	return sc, st
}

func summary(id string) homebox.EntitySummary {
	return homebox.EntitySummary{ID: id, Name: "Item " + id, UpdatedAt: time.Now().UTC()}
}
func detail(id, mfr, model string, atts ...homebox.Attachment) *homebox.EntityOut {
	return &homebox.EntityOut{ID: id, Name: "Item " + id, Manufacturer: mfr, ModelNumber: model, Attachments: atts}
}

// --- tests ---

func TestAttachHighConfidence(t *testing.T) {
	api := &fakeAPI{
		list:    []homebox.EntitySummary{summary("e1")},
		details: map[string]*homebox.EntityOut{"e1": detail("e1", "Acme", "W-1")},
	}
	disc := &fakeDisc{res: &discovery.Result{Best: &discovery.Candidate{URL: "http://x/m.pdf", ModelMatch: true}, Confidence: 0.9}, body: []byte("%PDF-1.4")}
	nt := &fakeNtfy{}
	sc, st := newTestScanner(t, api, disc, nt)
	defer st.Close()

	if err := sc.Scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if api.uploads != 1 {
		t.Fatalf("expected 1 upload, got %d", api.uploads)
	}
	rec, _ := st.Get(context.Background(), "e1")
	if rec == nil || rec.Status != store.StatusAttached {
		t.Fatalf("expected attached, got %+v", rec)
	}
}

func TestLowConfidenceReviewGate(t *testing.T) {
	api := &fakeAPI{
		list:    []homebox.EntitySummary{summary("e1")},
		details: map[string]*homebox.EntityOut{"e1": detail("e1", "Acme", "W-1")},
	}
	disc := &fakeDisc{res: &discovery.Result{Best: &discovery.Candidate{URL: "http://x/maybe.pdf"}, Confidence: 0.4}}
	nt := &fakeNtfy{}
	sc, st := newTestScanner(t, api, disc, nt)
	defer st.Close()

	if err := sc.Scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if api.uploads != 0 {
		t.Fatal("low confidence must not attach")
	}
	if nt.sent != 1 || api.patches != 1 {
		t.Fatalf("expected one ntfy + one tag patch, got sent=%d patches=%d", nt.sent, api.patches)
	}
	if api.lastTags[0] != "tag-unverified" {
		t.Fatalf("expected unverified tag applied, got %v", api.lastTags)
	}
	rec, _ := st.Get(context.Background(), "e1")
	if rec.Status != store.StatusPendingReview {
		t.Fatalf("expected pending_review, got %s", rec.Status)
	}
}

func TestSkipIfManualExists(t *testing.T) {
	api := &fakeAPI{
		list:    []homebox.EntitySummary{summary("e1")},
		details: map[string]*homebox.EntityOut{"e1": detail("e1", "Acme", "W-1", homebox.Attachment{Type: "manual"})},
	}
	disc := &fakeDisc{}
	sc, st := newTestScanner(t, api, disc, &fakeNtfy{})
	defer st.Close()

	if err := sc.Scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if disc.discCalls != 0 {
		t.Fatal("should not run discovery when a manual already exists")
	}
	rec, _ := st.Get(context.Background(), "e1")
	if rec.Status != store.StatusAttached {
		t.Fatalf("expected attached, got %s", rec.Status)
	}
}

func TestNoSearchableIdentity(t *testing.T) {
	// mfr, model AND name all empty => nothing to search on.
	api := &fakeAPI{
		list:    []homebox.EntitySummary{summary("e1")},
		details: map[string]*homebox.EntityOut{"e1": {ID: "e1", Name: ""}},
	}
	disc := &fakeDisc{}
	sc, st := newTestScanner(t, api, disc, &fakeNtfy{})
	defer st.Close()

	if err := sc.Scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if disc.discCalls != 0 {
		t.Fatal("no searchable identity => must not search")
	}
	rec, _ := st.Get(context.Background(), "e1")
	if rec.Status != store.StatusNotFound {
		t.Fatalf("expected notfound, got %s", rec.Status)
	}
}

func TestNameOnlySearches(t *testing.T) {
	// name present, no mfr/model => still searches (subject = name).
	api := &fakeAPI{
		list:    []homebox.EntitySummary{summary("e1")},
		details: map[string]*homebox.EntityOut{"e1": {ID: "e1", Name: "PlayStation Portal"}},
	}
	disc := &fakeDisc{res: &discovery.Result{Best: &discovery.Candidate{URL: "http://x/m.pdf"}, Confidence: 0.95}, body: []byte("%PDF")}
	sc, st := newTestScanner(t, api, disc, &fakeNtfy{})
	defer st.Close()

	if err := sc.Scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if disc.discCalls != 1 {
		t.Fatalf("name-only item should be searched, discCalls=%d", disc.discCalls)
	}
	if api.uploads != 1 {
		t.Fatalf("expected attach, uploads=%d", api.uploads)
	}
}

func TestDedupeSkipsReupload(t *testing.T) {
	ctx := context.Background()
	body := []byte("%PDF-1.4 identical")
	api := &fakeAPI{
		list:    []homebox.EntitySummary{summary("e1")},
		details: map[string]*homebox.EntityOut{"e1": detail("e1", "Acme", "W-1")},
	}
	disc := &fakeDisc{res: &discovery.Result{Best: &discovery.Candidate{URL: "http://x/m.pdf", ModelMatch: true}, Confidence: 0.9}, body: body}
	sc, st := newTestScanner(t, api, disc, &fakeNtfy{})
	defer st.Close()

	// Pre-seed a record with the same content hash, and an OLDER updatedAt so the
	// item is reprocessed (changed), exercising the dedupe branch.
	_ = st.Upsert(ctx, &store.Record{
		EntityID: "e1", Status: store.StatusAttached,
		DocSHA256: store.DocSHA(body), UpdatedAt: "2000-01-01T00:00:00Z",
	})

	if err := sc.Scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	if api.uploads != 0 {
		t.Fatalf("identical doc must not be re-uploaded, uploads=%d", api.uploads)
	}
	rec, _ := st.Get(ctx, "e1")
	if rec.Status != store.StatusAttached {
		t.Fatalf("expected attached, got %s", rec.Status)
	}
}
