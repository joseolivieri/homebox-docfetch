package llm

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strings"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// renderSticker draws a synthetic model-label PNG (white text on dark plate).
func renderSticker(t *testing.T, lines []string) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 480, 200))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{30, 30, 34, 255}}, image.Point{}, draw.Src)
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.White),
		Face: basicfont.Face7x13,
	}
	y := 40
	for _, l := range lines {
		d.Dot = fixed.P(24, y)
		d.DrawString(l)
		y += 28
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestLiveExtractIntake exercises the vision path against OpenRouter for real.
// Skipped unless OPENROUTER_API_KEY is set.
func TestLiveExtractIntake(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("set OPENROUTER_API_KEY to run")
	}
	model := os.Getenv("VISION_MODEL")
	if model == "" {
		model = "google/gemini-2.5-flash-lite"
	}
	c := New("https://openrouter.ai/api/v1", key, "unused-for-vision")

	sticker := renderSticker(t, []string{
		"SONY",
		"PlayStation Portal Remote Player",
		"Model No: CFI-Y1000",
		"Serial No: S01-4567890-A",
		"DC 5V  1.5A   Made in China",
	})
	ex, err := c.ExtractIntake(context.Background(), model, []IntakeImage{{Data: sticker, Mime: "image/png"}})
	if err != nil {
		t.Fatalf("extract intake: %v", err)
	}
	t.Logf("extraction: %+v", ex)
	if !strings.Contains(strings.ToUpper(ex.Sticker.ModelNumber), "CFI-Y1000") {
		t.Fatalf("expected CFI-Y1000, got %q", ex.Sticker.ModelNumber)
	}
	if !strings.Contains(strings.ToLower(ex.Sticker.Manufacturer), "sony") {
		t.Fatalf("expected Sony, got %q", ex.Sticker.Manufacturer)
	}
	if !strings.Contains(ex.Sticker.SerialNumber, "4567890") {
		t.Fatalf("expected serial, got %q", ex.Sticker.SerialNumber)
	}
}
