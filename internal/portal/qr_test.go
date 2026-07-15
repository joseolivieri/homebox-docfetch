package portal

import (
	"bytes"
	"image/png"
	"testing"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"

	"github.com/joseolivieri/homebox-docfetch/internal/llm"
)

func qrPNG(t *testing.T, content string) []byte {
	t.Helper()
	w := qrcode.NewQRCodeWriter()
	m, err := w.Encode(content, gozxing.BarcodeFormat_QR_CODE, 256, 256, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecodeQRs(t *testing.T) {
	imgs := []llm.IntakeImage{
		{Data: qrPNG(t, "https://support.anker.com/s/article/A1289"), Mime: "image/png"},
		{Data: qrPNG(t, "https://play.google.com/store/apps/details?id=x"), Mime: "image/png"}, // filtered
		{Data: qrPNG(t, "WIFI:S:home;P:pass;;"), Mime: "image/png"},                            // not a URL
		{Data: []byte("not an image"), Mime: "image/jpeg"},                                     // ignored
	}
	got := decodeQRs(imgs)
	if len(got) != 1 || got[0] != "https://support.anker.com/s/article/A1289" {
		t.Fatalf("decodeQRs = %v", got)
	}
}
