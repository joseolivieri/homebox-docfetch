package discovery

import (
	"bytes"
	"context"
	"log"
	"strings"

	"github.com/ledongthuc/pdf"

	"github.com/joseolivieri/homebox-docfetch/internal/llm"
)

// Verifier reads a document excerpt into structured identity facts (satisfied
// by *llm.Client.SkimDoc). Optional — nil disables LLM-level skimming; the
// rules-first model scan still works.
type Verifier interface {
	SkimDoc(ctx context.Context, itemDesc, wantClass, excerpt string) (*llm.DocSkim, error)
}

// SetVerifier enables content-level document skimming.
func (e *Engine) SetVerifier(v Verifier) { e.verifier = v }

// SkimVerdict is the content-level evidence extracted from a downloaded doc.
// It serves BOTH directions: veto (ProductMismatch/ClassMismatch reject an
// otherwise-confident pick) and promote (ModelConfirmed lifts a candidate the
// URL heuristics couldn't confirm — manufacturer sites often use internal doc
// numbers like W10903644 in URLs, while the cover page names the real model).
type SkimVerdict struct {
	IsPDF           bool
	HasText         bool // false for scanned/image-only PDFs — skim inconclusive
	ModelConfirmed  bool // the item's model number appears in the document text
	ClassMismatch   bool // doc is clearly a different class (parts list vs manual)
	ProductMismatch bool // doc positively identifies a DIFFERENT product
	// TextReason explains an empty read ("not-pdf", "parse-error", "no-text").
	// Extraction failure used to be indistinguishable from a clean read; the
	// scanner logs this as a skim.unreadable event so the rate is measurable.
	TextReason string
}

// Skim inspects downloaded document bytes. Rules first: a direct scan of the
// opening pages for the item's model number needs no LLM. The LLM skim runs
// only when the verifier is set and either the model wasn't rules-confirmed
// or a class check is wanted.
func (e *Engine) Skim(ctx context.Context, it Item, data []byte, wantClass string) SkimVerdict {
	v := SkimVerdict{}
	// Hard requirement: the bytes must actually be a PDF. Rules only see
	// URL/HEAD hints — observed live: a manualslib .html page won the rerank
	// and got attached as a "manual".
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		log.Printf("skim: downloaded content is not a PDF")
		v.TextReason = "not-pdf"
		return v
	}
	v.IsPDF = true

	text, reason := pdfText(data, 6, 3, 16_000)
	if strings.TrimSpace(text) == "" {
		// Scanned/image-only PDF, or an extractor failure. Either way the
		// content is INCONCLUSIVE, not verified: skimAccepts routes
		// non-official sources to a human instead of attaching blind (R4).
		v.TextReason = reason
		return v
	}
	v.HasText = true

	// Rules-first: the model number in the document text is definitive
	// evidence and costs nothing.
	if modelInText(it.ModelNumber, text) {
		v.ModelConfirmed = true
	}

	if e.verifier == nil {
		return v
	}
	excerpt := text
	if len(excerpt) > 1500 {
		excerpt = excerpt[:1500]
	}
	skim, err := e.verifier.SkimDoc(ctx, it.desc(), wantClass, excerpt)
	if err != nil {
		log.Printf("skim: llm error (%v) — rules verdict only", err)
		return v
	}
	if skim.DifferentProduct {
		v.ProductMismatch = true
	}
	if !v.ModelConfirmed {
		for _, m := range skim.Models {
			if modelsEquivalent(it.ModelNumber, m) {
				v.ModelConfirmed = true
				break
			}
		}
	}
	// Class check: only flag a mismatch on a positive different-class read.
	// "other"/"" stays inconclusive.
	if wantClass != "" && skim.DocType != "" && skim.DocType != "other" && skim.DocType != wantClass {
		v.ClassMismatch = true
	}
	return v
}

// modelInText reports whether the (normalized) model number appears in the
// text, tolerating trailing revision suffixes — appliance docs print the
// family (WDF520PADM) while the label carries a revision (WDF520PADM7).
func modelInText(model, text string) bool {
	m := norm(model)
	if len(m) < 4 {
		return false
	}
	t := norm(text)
	if strings.Contains(t, m) {
		return true
	}
	if len(m) >= 7 && strings.Contains(t, m[:len(m)-1]) {
		return true
	}
	if len(m) >= 8 && strings.Contains(t, m[:len(m)-2]) {
		return true
	}
	return false
}

// modelsEquivalent compares the item's model to one the LLM extracted.
func modelsEquivalent(itemModel, docModel string) bool {
	a, b := norm(itemModel), norm(docModel)
	if len(a) < 4 || len(b) < 4 {
		return false
	}
	return strings.Contains(a, b) || strings.Contains(b, a)
}

// pdfText pulls up to maxChars of text from a PDF's first `first` pages AND
// its last `last` pages — "models covered" tables live on back covers and in
// end-of-document spec tables as often as on the cover (R3). Returns the text
// plus a reason string that is empty on success and otherwise explains why
// nothing came out ("parse-error", "no-text").
func pdfText(data []byte, first, last, maxChars int) (text string, reason string) {
	reason = "parse-error"
	defer func() {
		if p := recover(); p != nil { // the pdf lib panics on malformed files
			text, reason = "", "parse-error"
		}
	}()

	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", "parse-error"
	}
	total := r.NumPage()

	// Page set: 1..first, plus the trailing `last` pages, de-duplicated for
	// short documents where the two ranges overlap.
	want := map[int]bool{}
	var order []int
	add := func(p int) {
		if p >= 1 && p <= total && !want[p] {
			want[p] = true
			order = append(order, p)
		}
	}
	for p := 1; p <= first; p++ {
		add(p)
	}
	for p := total - last + 1; p <= total; p++ {
		add(p)
	}

	var b strings.Builder
	for _, p := range order {
		if b.Len() >= maxChars {
			break
		}
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
	if out == "" {
		return "", "no-text" // parsed fine, but the pages carry no text layer
	}
	return out, ""
}
