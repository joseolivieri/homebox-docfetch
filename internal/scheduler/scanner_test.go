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
func (f *fakeAPI) DownloadAttachment(_ context.Context, entityID, attachmentID string, maxBytes int64) ([]byte, string, error) {
	return nil, "", nil
}
func (f *fakeAPI) EnsureTag(_ context.Context, name string) (string, error) {
	return "tag-unverified", nil
}

type fakeDisc struct {
	res         *discovery.Result
	body        []byte
	discCalls   int
	skimConfirm bool // Skim reports the model confirmed in document text
}

func (d *fakeDisc) Discover(_ context.Context, _ discovery.Item, _ []string) (*discovery.Result, error) {
	d.discCalls++
	return d.res, nil
}
func (d *fakeDisc) SelectClass(_ context.Context, _ discovery.Item, _ []discovery.Candidate, _ string) *discovery.Result {
	if d.res == nil {
		return &discovery.Result{}
	}
	return d.res
}
func (d *fakeDisc) Download(_ context.Context, _ string, _ int64) ([]byte, error) { return d.body, nil }
func (d *fakeDisc) Skim(_ context.Context, _ discovery.Item, data []byte, _ string) discovery.SkimVerdict {
	return discovery.SkimVerdict{
		IsPDF:          len(data) >= 4 && string(data[:4]) == "%PDF",
		HasText:        true,
		ModelConfirmed: d.skimConfirm,
	}
}

type fakeNtfy struct{ sent int }

func (n *fakeNtfy) Send(_ context.Context, _ notify.Message) error { n.sent++; return nil }

// --- helpers ---

func newTestScanner(t *testing.T, api *fakeAPI, disc *fakeDisc, nt *fakeNtfy) (*Scanner, *store.Store) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	sc := NewScanner(api, disc, nt, st, Config{
		DocsEnabled:         true,
		AutoAttachThreshold: 0.7,
		SkipIfExists:        true,
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

func TestCategoryMatchAndDocName(t *testing.T) {
	appliance := &homebox.EntityOut{Tags: []homebox.Tag{{Name: "Dishwasher"}}}
	earbuds := &homebox.EntityOut{Tags: []homebox.Tag{{Name: "Earbuds"}}}
	byName := &homebox.EntityOut{Name: "Whirlpool WDF520PADM7 Dishwasher"} // no category tag
	parts := DocClassCfg{Name: "parts", Field: "Parts", Categories: []string{"dishwasher", "washer"}}
	if !categoryMatch(appliance, parts.Categories) {
		t.Fatal("dishwasher tag should match parts categories")
	}
	if !categoryMatch(byName, parts.Categories) {
		t.Fatal("dishwasher in the NAME should match parts categories")
	}
	if categoryMatch(earbuds, parts.Categories) {
		t.Fatal("earbuds must NOT match parts categories")
	}
	if categoryMatch(earbuds, nil) != true {
		t.Fatal("empty categories = applies to all")
	}
	// non-manual class gets a filename suffix so it can't collide with the manual
	d := &homebox.EntityOut{Manufacturer: "LG", ModelNumber: "DLEX4000"}
	if got := filename(d, parts); got != "LG-DLEX4000-parts.pdf" {
		t.Fatalf("parts filename = %q", got)
	}
	man := DocClassCfg{Name: "manual", Field: "Manual"}
	if got := filename(d, man); got != "LG-DLEX4000.pdf" {
		t.Fatalf("manual filename = %q", got)
	}
}

func TestHasDocByField(t *testing.T) {
	parts := DocClassCfg{Name: "parts", Field: "Parts", AttachAs: "attachment"}
	withField := &homebox.EntityOut{Fields: []homebox.EntityField{{Name: "Parts", TextValue: "[pdf](x)"}}}
	if !hasDoc(withField, parts) {
		t.Fatal("field presence should satisfy hasDoc")
	}
	if hasDoc(&homebox.EntityOut{}, parts) {
		t.Fatal("empty entity should not satisfy hasDoc")
	}
	// manual back-compat: a bare type=manual attachment counts even without a field
	man := DocClassCfg{Name: "manual", Field: "Manual"}
	legacy := &homebox.EntityOut{Attachments: []homebox.Attachment{{Type: "manual"}}}
	if !hasDoc(legacy, man) {
		t.Fatal("legacy manual attachment should satisfy hasDoc(manual)")
	}
}

func TestReviewGateNotifiesOnce(t *testing.T) {
	// Our own writes bump updatedAt and re-trigger scans; the review prompt for
	// the SAME candidate URL must not repeat (observed live: one ntfy per poll
	// tick until fixed).
	ctx := context.Background()
	api := &fakeAPI{
		list:    []homebox.EntitySummary{summary("e1")},
		details: map[string]*homebox.EntityOut{"e1": detail("e1", "Acme", "W-1")},
	}
	disc := &fakeDisc{res: &discovery.Result{Best: &discovery.Candidate{URL: "http://x/maybe.pdf"}, Confidence: 0.4}}
	nt := &fakeNtfy{}
	sc, st := newTestScanner(t, api, disc, nt)
	defer st.Close()

	if err := sc.Scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	// Simulate an updatedAt bump (our own tag PATCH / notes PUT) -> rescan.
	api.list[0].UpdatedAt = api.list[0].UpdatedAt.Add(time.Minute)
	if err := sc.Scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	if nt.sent != 1 {
		t.Fatalf("review prompt must fire once, got %d", nt.sent)
	}
	if api.patches != 1 {
		t.Fatalf("unverified tag must be patched once, got %d", api.patches)
	}
	rec, _ := st.Get(ctx, "e1")
	if rec.Status != store.StatusPendingReview {
		t.Fatalf("expected pending_review, got %s", rec.Status)
	}
}

func TestSkimPromotesGatedCandidate(t *testing.T) {
	// Model-gated pick (confidence zeroed: model number absent from the URL)
	// whose document TEXT names the model -> attach without human review.
	// This is the Whirlpool case: URLs carry doc numbers (W10903644), the
	// cover page carries the model.
	api := &fakeAPI{
		list:    []homebox.EntitySummary{summary("e1")},
		details: map[string]*homebox.EntityOut{"e1": detail("e1", "Whirlpool", "WDF520PADM7")},
	}
	best := discovery.Candidate{URL: "http://w.com/owners-manual-W10903644.pdf", IsPDF: true, Score: 3}
	disc := &fakeDisc{
		res:         &discovery.Result{Best: &best, Candidates: []discovery.Candidate{best}, Confidence: 0},
		body:        []byte("%PDF-1.4 use and care guide"),
		skimConfirm: true,
	}
	nt := &fakeNtfy{}
	sc, st := newTestScanner(t, api, disc, nt)
	defer st.Close()

	if err := sc.Scan(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if api.uploads != 1 {
		t.Fatalf("skim-confirmed doc must attach, uploads=%d", api.uploads)
	}
	if nt.sent != 0 {
		t.Fatalf("no review prompt when skim confirms, sent=%d", nt.sent)
	}
	rec, _ := st.Get(context.Background(), "e1")
	if rec.Status != store.StatusAttached {
		t.Fatalf("expected attached, got %s", rec.Status)
	}
}
