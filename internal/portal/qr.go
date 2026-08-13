package portal

import (
	"bytes"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"strings"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/datamatrix"
	"github.com/makiuchi-d/gozxing/multi/qrcode"
	"github.com/makiuchi-d/gozxing/oned"
	_ "golang.org/x/image/webp"

	"github.com/joseolivieri/homebox-docfetch/internal/llm"
)

// DecodedCode is one code read off an intake photo, already classified.
type DecodedCode struct {
	Format  string // QR | DataMatrix | EAN13 | UPCA | …
	Payload Payload
}

// decodeCodes reads every machine-readable code in the intake photos: QR
// (support links), DataMatrix (GS1 element strings, Amazon Transparency) and
// 1D barcodes (UPC/EAN → GTIN). Decoding is pure-local — no network, no LLM —
// so it respects the intake stage's vision-only egress boundary; curation
// follows whatever is chaseable.
func decodeCodes(images []llm.IntakeImage) []DecodedCode {
	seen := map[string]bool{}
	var out []DecodedCode
	hints := map[gozxing.DecodeHintType]any{gozxing.DecodeHintType_TRY_HARDER: true}

	qr := qrcode.NewQRCodeMultiReader()
	dm := datamatrix.NewDataMatrixReader()
	upc := oned.NewMultiFormatUPCEANReader(hints)
	c128 := oned.NewCode128Reader()

	for _, im := range images {
		img, _, err := image.Decode(bytes.NewReader(im.Data))
		if err != nil {
			continue
		}
		bmp, err := gozxing.NewBinaryBitmapFromImage(img)
		if err != nil {
			continue
		}

		var raws []struct{ format, text string }
		// QR can carry several codes in one frame; the rest read one each.
		if results, err := qr.DecodeMultiple(bmp, hints); err == nil {
			for _, r := range results {
				raws = append(raws, struct{ format, text string }{"QR", r.GetText()})
			}
		}
		for _, rd := range []struct {
			name   string
			reader gozxing.Reader
		}{{"DataMatrix", dm}, {"UPC/EAN", upc}, {"Code128", c128}} {
			// Each reader consumes the bitmap's decode state; reset per try.
			b2, err := gozxing.NewBinaryBitmapFromImage(img)
			if err != nil {
				continue
			}
			if r, err := rd.reader.Decode(b2, hints); err == nil && r != nil {
				raws = append(raws, struct{ format, text string }{rd.name, r.GetText()})
			}
		}

		for _, raw := range raws {
			txt := strings.TrimSpace(raw.text)
			if txt == "" || seen[txt] {
				continue
			}
			seen[txt] = true
			p := ClassifyPayload(txt)
			if p.Kind == PayloadNonWeb {
				continue // WIFI:/mailto:/tel: — nothing to keep
			}
			if p.Kind == PayloadSupportURL && !usableQRURL(p.URL) {
				continue // app-store / payment / social noise
			}
			out = append(out, DecodedCode{Format: raw.format, Payload: p})
			log.Printf("intake: %s decoded (%s) -> %s", raw.format, p.Kind, txt)
		}
	}
	return out
}

// usableQRURL keeps http(s) links that could plausibly lead to product
// support. App-store installs, social links, and payment codes are noise.
// Video platforms (YouTube/Vimeo) pass: makers print QR codes to their
// channel/support videos (observed live: a water timer's QR → the company's
// YouTube page), and those links are provenance for the future
// maintenance-resources milestone even though the doc pipeline skips them.
func usableQRURL(u string) bool {
	l := strings.ToLower(u)
	if !strings.HasPrefix(l, "http://") && !strings.HasPrefix(l, "https://") {
		return false // wifi:, mailto:, plain-text payloads
	}
	for _, bad := range []string{
		"play.google.com", "apps.apple.com", "itunes.apple.com", "onelink.me",
		"app.adjust.com", "facebook.com", "instagram.com", "twitter.com",
		"x.com/", "tiktok.com", "wa.me", "t.me",
		"paypal.", "venmo.", "cash.app",
	} {
		if strings.Contains(l, bad) {
			return false
		}
	}
	return true
}
