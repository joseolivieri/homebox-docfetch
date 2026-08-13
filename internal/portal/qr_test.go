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

func TestDecodeCodesClassifiesAndFilters(t *testing.T) {
	imgs := []llm.IntakeImage{
		{Data: qrPNG(t, "https://support.anker.com/s/article/A1289"), Mime: "image/png"},
		{Data: qrPNG(t, "https://play.google.com/store/apps/details?id=x"), Mime: "image/png"}, // filtered
		{Data: qrPNG(t, "WIFI:S:home;P:pass;;"), Mime: "image/png"},                            // dropped: non-web
		{Data: qrPNG(t, "https://id.gs1.org/01/09520123456788/21/UNIT42"), Mime: "image/png"},  // digital link
		{Data: qrPNG(t, "https://www.youtube.com/@AcmeTimers"), Mime: "image/png"},             // provenance only
		{Data: []byte("not an image"), Mime: "image/jpeg"},                                     // ignored
	}
	got := decodeCodes(imgs)

	byKind := map[PayloadKind]Payload{}
	for _, c := range got {
		byKind[c.Payload.Kind] = c.Payload
	}

	sup, ok := byKind[PayloadSupportURL]
	if !ok || sup.URL != "https://support.anker.com/s/article/A1289" {
		t.Fatalf("support URL not decoded: %+v", got)
	}
	if !sup.Chaseable() {
		t.Fatal("a support URL must be chaseable")
	}

	dl, ok := byKind[PayloadDigitalLink]
	if !ok || dl.GTIN != "09520123456788" || dl.Serial != "UNIT42" {
		t.Fatalf("GS1 Digital Link not parsed: %+v", dl)
	}

	plat, ok := byKind[PayloadPlatform]
	if !ok {
		t.Fatal("platform link should be kept as provenance")
	}
	if plat.Chaseable() {
		t.Fatal("platform links must never be chased")
	}

	if _, bad := byKind[PayloadNonWeb]; bad {
		t.Fatal("WIFI: payload should have been dropped")
	}
	for _, c := range got {
		if c.Payload.Raw == "https://play.google.com/store/apps/details?id=x" {
			t.Fatal("app-store link should have been filtered")
		}
	}
}

func TestClassifyPayload(t *testing.T) {
	cases := []struct {
		raw    string
		kind   PayloadKind
		gtin   string
		serial string
		chase  bool
	}{
		{"https://acme.example/support/wt41", PayloadSupportURL, "", "", true},
		{"https://id.gs1.org/01/09520123456788/21/AB1", PayloadDigitalLink, "09520123456788", "AB1", true},
		{"(01)09520123456788(21)SER99", PayloadElement, "09520123456788", "SER99", false},
		{"AZABCDEFGHIJKLMNOPQRSTUVWXYZ", PayloadAuthToken, "", "", false},
		{"MT:Y.K9042C00KA0648G00", PayloadMatter, "", "", false},
		{"WIFI:S:home;P:pass;;", PayloadNonWeb, "", "", false},
		{"https://youtu.be/abc", PayloadPlatform, "", "", false},
	}
	for _, c := range cases {
		p := ClassifyPayload(c.raw)
		if p.Kind != c.kind || p.GTIN != c.gtin || p.Serial != c.serial || p.Chaseable() != c.chase {
			t.Errorf("ClassifyPayload(%q) = %+v chase=%v; want kind=%s gtin=%q serial=%q chase=%v",
				c.raw, p, p.Chaseable(), c.kind, c.gtin, c.serial, c.chase)
		}
	}
}
