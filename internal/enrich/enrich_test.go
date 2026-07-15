package enrich

import (
	"context"
	"strings"
	"testing"

	"github.com/joseolivieri/homebox-docfetch/internal/llm"
)

type fakeSearch struct {
	byQuery map[string][]SearchResult
	calls   []string
}

func (f *fakeSearch) Search(_ context.Context, q string) ([]SearchResult, error) {
	f.calls = append(f.calls, q)
	for k, v := range f.byQuery {
		if strings.Contains(strings.ToLower(q), strings.ToLower(k)) {
			return v, nil
		}
	}
	return nil, nil
}

type fakeExtract struct{ id *llm.Identity }

func (f *fakeExtract) ExtractIdentity(_ context.Context, _ string, _ []llm.Candidate) (*llm.Identity, error) {
	return f.id, nil
}

func sonyResults() []SearchResult {
	return []SearchResult{
		{Title: "PlayStation Portal Remote Player CFI-Y1000", URL: "https://playstation.com/portal", Snippet: "Sony CFI-Y1000 remote player"},
		{Title: "Sony PlayStation Portal review (CFI-Y1000)", URL: "https://theverge.com/rev", Snippet: "Sony's CFI-Y1000 portal"},
		{Title: "Portal accessories", URL: "https://shop.example.com/x", Snippet: "unrelated"},
	}
}

func engine(fs *fakeSearch, fx *fakeExtract) *Engine {
	return New(Options{
		Enabled:            true,
		FillOnly:           true,
		AutoWriteThreshold: 0.85,
		MinAgreeingSources: 2,
		BackCheck:          true,
	}, fs, fx)
}

func TestWriteWhenCorroborated(t *testing.T) {
	fs := &fakeSearch{byQuery: map[string][]SearchResult{
		"playstation portal": sonyResults(), // forward
		"cfi-y1000":          sonyResults(), // back-check round trip mentions "PlayStation Portal"
	}}
	fx := &fakeExtract{id: &llm.Identity{
		Manufacturer: "Sony", ModelNumber: "CFI-Y1000", Name: "PlayStation Portal Remote Player",
		Confidence: map[string]float64{"manufacturer": 0.95, "modelNumber": 0.9, "name": 0.9, "category": 0.4},
	}}
	frs, err := engine(fs, fx).Enrich(context.Background(), Item{Name: "PlayStation Portal"}, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Action{}
	for _, f := range frs {
		got[f.Field] = f.Action
	}
	if got["manufacturer"] != ActionWrite {
		t.Fatalf("manufacturer should write (2 domains agree + back-check), got %v", got["manufacturer"])
	}
	if got["modelNumber"] != ActionWrite {
		t.Fatalf("modelNumber should write, got %v", got["modelNumber"])
	}
}

func TestReviewWhenSingleSource(t *testing.T) {
	one := []SearchResult{
		{Title: "Sony PlayStation Portal CFI-Y1000", URL: "https://playstation.com/p", Snippet: "official"},
		{Title: "some blog", URL: "https://blog.io/x", Snippet: "no model here"},
	}
	fs := &fakeSearch{byQuery: map[string][]SearchResult{
		"playstation portal": one,
		"cfi-y1000":          one,
	}}
	fx := &fakeExtract{id: &llm.Identity{
		Manufacturer: "Sony", ModelNumber: "CFI-Y1000",
		Confidence: map[string]float64{"manufacturer": 0.9, "modelNumber": 0.9},
	}}
	frs, err := engine(fs, fx).Enrich(context.Background(), Item{Name: "PlayStation Portal"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range frs {
		if f.Field == "modelNumber" && f.Action == ActionWrite {
			t.Fatal("single-source model number must not auto-write")
		}
	}
}

func TestBackCheckFailBlocksWrite(t *testing.T) {
	fs := &fakeSearch{byQuery: map[string][]SearchResult{
		"playstation portal": sonyResults(),
		"cfi-y1000":          {{Title: "totally different product", URL: "https://x.com", Snippet: "vacuum cleaner"}},
	}}
	fx := &fakeExtract{id: &llm.Identity{
		Manufacturer: "Sony", ModelNumber: "CFI-Y1000",
		Confidence: map[string]float64{"manufacturer": 0.95, "modelNumber": 0.95},
	}}
	frs, err := engine(fs, fx).Enrich(context.Background(), Item{Name: "PlayStation Portal"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range frs {
		if f.Action == ActionWrite {
			t.Fatalf("back-check failure must block writes, %s wrote", f.Field)
		}
	}
}

func TestModelSanity(t *testing.T) {
	for v, want := range map[string]bool{
		"CFI-Y1000":                 true,
		"WH-1000XM5":                true,
		"Ultimate Gaming Companion": false,
		"X":                         false,
	} {
		if modelSane(v) != want {
			t.Fatalf("modelSane(%q) != %v", v, want)
		}
	}
}

func TestFillOnlyGaps(t *testing.T) {
	e := engine(&fakeSearch{}, &fakeExtract{})
	gaps := e.gaps(Item{Manufacturer: "Sony", Name: "PlayStation Portal"})
	for _, g := range gaps {
		if g == "manufacturer" || g == "name" {
			t.Fatalf("non-empty field %q offered as gap", g)
		}
	}
	found := false
	for _, g := range gaps {
		if g == "modelNumber" {
			found = true
		}
	}
	if !found {
		t.Fatal("empty modelNumber should be a gap")
	}
}
