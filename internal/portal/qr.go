package portal

import (
	"bytes"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"strings"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/multi/qrcode"
	_ "golang.org/x/image/webp"

	"github.com/joseolivieri/homebox-docfetch/internal/llm"
)

// decodeQRs extracts http(s) URLs from QR codes in the intake photos —
// manufacturers print support-page QR codes on model labels and warranty
// cards. Decoding is pure-local (no network, no LLM), so it respects the
// intake stage's vision-only egress boundary; the scanner follows the links.
func decodeQRs(images []llm.IntakeImage) []string {
	seen := map[string]bool{}
	var out []string
	reader := qrcode.NewQRCodeMultiReader()
	hints := map[gozxing.DecodeHintType]any{gozxing.DecodeHintType_TRY_HARDER: true}
	for _, im := range images {
		img, _, err := image.Decode(bytes.NewReader(im.Data))
		if err != nil {
			continue
		}
		bmp, err := gozxing.NewBinaryBitmapFromImage(img)
		if err != nil {
			continue
		}
		results, err := reader.DecodeMultiple(bmp, hints)
		if err != nil {
			continue // no QR in this photo — normal
		}
		for _, r := range results {
			u := strings.TrimSpace(r.GetText())
			if !usableQRURL(u) || seen[u] {
				continue
			}
			seen[u] = true
			out = append(out, u)
			log.Printf("intake: QR decoded -> %s", u)
		}
	}
	return out
}

// usableQRURL keeps http(s) links that could plausibly lead to product
// support. App-store installs, social links, and payment codes are noise.
func usableQRURL(u string) bool {
	l := strings.ToLower(u)
	if !strings.HasPrefix(l, "http://") && !strings.HasPrefix(l, "https://") {
		return false // wifi:, mailto:, plain-text payloads
	}
	for _, bad := range []string{
		"play.google.com", "apps.apple.com", "itunes.apple.com", "onelink.me",
		"app.adjust.com", "facebook.com", "instagram.com", "twitter.com",
		"x.com/", "youtube.com", "tiktok.com", "wa.me", "t.me",
		"paypal.", "venmo.", "cash.app",
	} {
		if strings.Contains(l, bad) {
			return false
		}
	}
	return true
}
