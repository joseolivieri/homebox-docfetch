package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// PickProductImage asks the vision model which candidate image best depicts the
// product. When reference is non-nil (the user's own photo of the item from
// intake), candidates are matched against it — a poor man's reverse image
// search. Returns the zero-based candidate index (-1 if none fit) and a
// confidence the caller thresholds.
func (c *Client) PickProductImage(ctx context.Context, visionModel, subject string, reference *IntakeImage, candidates []IntakeImage) (int, float64, error) {
	if len(candidates) == 0 {
		return -1, 0, nil
	}
	if visionModel == "" {
		return -1, 0, fmt.Errorf("no vision model configured")
	}

	sys := "You pick the best OFFICIAL product image for an inventory item. Prefer clean, " +
		"catalog-style images that clearly show the exact product; reject unrelated products, " +
		"collages, screenshots, memes, and images of a different model. "
	prompt := fmt.Sprintf("Product: %s\n", subject)
	if reference != nil {
		sys += "The FIRST image is the user's own photo of the actual item — it is the ground truth; " +
			"pick the candidate showing the SAME product. "
		prompt += "Image 1 is the reference photo of the actual item. The following images are candidates 0..N in order.\n"
	} else {
		prompt += "The images are candidates 0..N in order.\n"
	}
	sys += `Respond ONLY with JSON: {"best":<candidate index or -1 if none acceptable>,"confidence":<0.0-1.0>}. No prose.`

	parts := []mmPart{{Type: "text", Text: prompt}}
	appendImg := func(img IntakeImage) {
		mime := img.Mime
		if mime == "" {
			mime = "image/jpeg"
		}
		parts = append(parts, mmPart{
			Type:     "image_url",
			ImageURL: &mmImageURL{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)},
		})
	}
	if reference != nil {
		appendImg(*reference)
	}
	for _, cand := range candidates {
		appendImg(cand)
	}

	body, _ := json.Marshal(mmReq{
		Model:       visionModel,
		MaxTokens:   40,
		Temperature: 0,
		Messages: []mmMessage{
			{Role: "system", Content: sys},
			{Role: "user", Content: parts},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return -1, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return -1, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16384))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return -1, 0, fmt.Errorf("openrouter http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	content, err := extractContent(raw)
	if err != nil {
		return -1, 0, err
	}
	m := jsonObj.FindString(content)
	if m == "" {
		return -1, 0, fmt.Errorf("no JSON in image-pick reply: %q", content)
	}
	var out struct {
		Best       int     `json:"best"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(m), &out); err != nil {
		return -1, 0, err
	}
	if out.Best < -1 || out.Best >= len(candidates) {
		out.Best = -1
	}
	if out.Confidence < 0 {
		out.Confidence = 0
	} else if out.Confidence > 1 {
		out.Confidence = 1
	}
	return out.Best, out.Confidence, nil
}
