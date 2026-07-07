package discovery

import (
	"bytes"
	"context"
	"log"
	"strings"

	"github.com/ledongthuc/pdf"
)

// Verifier checks a document excerpt against an item identity (satisfied by
// *llm.Client.VerifyDoc). Optional — nil disables content verification.
type Verifier interface {
	VerifyDoc(ctx context.Context, itemDesc, excerpt string) (bool, float64, error)
}

// SetVerifier enables content-level document verification.
func (e *Engine) SetVerifier(v Verifier) { e.verifier = v }

// VerifyPDF extracts the opening text of a downloaded PDF and asks the
// verifier whether it belongs to the item. Returns:
//
//	true  — verified match, or verification impossible (no verifier, scanned/
//	        image-only PDF with no extractable text): benefit of the doubt,
//	        the search-layer gates already passed.
//	false — extractable text that the verifier says is a DIFFERENT product.
func (e *Engine) VerifyPDF(ctx context.Context, it Item, data []byte) bool {
	// Hard requirement regardless of verifier: the bytes must actually be a PDF.
	// Rules only see URL/HEAD hints — observed live: a manualslib .html page
	// won the rerank and got attached as a "manual".
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		log.Printf("verify: downloaded content is not a PDF — rejecting")
		return false
	}
	if e.verifier == nil {
		return true
	}
	excerpt := pdfExcerpt(data, 1200)
	if strings.TrimSpace(excerpt) == "" {
		log.Printf("verify: no extractable text (scanned pdf?) — allowing")
		return true
	}
	match, conf, err := e.verifier.VerifyDoc(ctx, it.desc(), excerpt)
	if err != nil {
		log.Printf("verify: error (%v) — allowing", err)
		return true
	}
	if !match {
		log.Printf("verify: content mismatch for %q (conf=%.2f) — rejecting doc", it.desc(), conf)
	}
	return match
}

// pdfExcerpt pulls up to maxChars of text from the first pages of a PDF.
// Returns "" when the PDF has no extractable text (scans) or parsing fails.
func pdfExcerpt(data []byte, maxChars int) string {
	defer func() { _ = recover() }() // the pdf lib can panic on malformed files

	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ""
	}
	var b strings.Builder
	pages := r.NumPage()
	if pages > 3 {
		pages = 3
	}
	for p := 1; p <= pages && b.Len() < maxChars; p++ {
		page := r.Page(p)
		if page.V.IsNull() {
			continue
		}
		txt, err := page.GetPlainText(nil)
		if err != nil {
			continue
		}
		b.WriteString(txt)
		b.WriteString("\n")
	}
	out := strings.Join(strings.Fields(b.String()), " ") // collapse whitespace
	if len(out) > maxChars {
		out = out[:maxChars]
	}
	return out
}
