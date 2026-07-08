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

// PickProductImage asks the vision model which candidate image best depicts
// the product, selecting by PRODUCT IDENTITY (manufacturer/model/category
// text), not by similarity to any photo. The user's own photo, when present,
// is only a tie-breaker for variant details like color — angle, open/closed
// state, and framing differences are expected and must not drive the choice
// (similarity anchoring once picked a charger over the actual earbuds).
// Returns the zero-based candidate index (-1 if none fit) and a confidence
// the caller thresholds.
func (c *Client) PickProductImage(ctx context.Context, visionModel, subject, category string, reference *IntakeImage, candidates []IntakeImage) (int, float64, error) {
	if len(candidates) == 0 {
		return -1, 0, nil
	}
	if visionModel == "" {
		return -1, 0, fmt.Errorf("no vision model configured")
	}

	sys := "You pick the best OFFICIAL catalog image for an inventory item. Decide by PRODUCT IDENTITY: " +
		"the image must show exactly the named product, alone, catalog-style (clean background preferred). " +
		"REJECT: accessories sold with or for it (chargers, cables, cases, stands), bundles, packaging-only " +
		"shots, collages, screenshots, and any different model — even a similar-looking one. " +
		"If no candidate clearly shows the named product itself, answer -1. "
	prompt := fmt.Sprintf("Product: %s\n", subject)
	if category != "" {
		prompt += fmt.Sprintf("Product type: %s — the chosen image must depict this type of object.\n", category)
	}
	if reference != nil {
		sys += "The FIRST image is the user's own photo of the item. Use it ONLY to break ties between " +
			"candidates that already match the named product (e.g. color variant). Differences in angle, " +
			"open/closed state, or framing are irrelevant. "
		prompt += "Image 1 is the user's reference photo (tie-break only). The following images are candidates 0..N in order.\n"
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

// VerifyProductImage is a single yes/no check: does this image show exactly
// the named product? Used on og:image candidates — their provenance (the
// manufacturer's own product page) is already strong, so one cheap sanity
// call replaces the whole search+rank path.
func (c *Client) VerifyProductImage(ctx context.Context, visionModel, subject, category string, img IntakeImage) (bool, float64, error) {
	if visionModel == "" {
		return false, 0, fmt.Errorf("no vision model configured")
	}
	sys := "You verify product images for an inventory. Answer whether the image shows exactly the named " +
		"product (the product itself — not just an accessory, box, or a different model). " +
		`Respond ONLY with JSON: {"match":true|false,"confidence":<0.0-1.0>}. No prose.`
	prompt := fmt.Sprintf("Product: %s\n", subject)
	if category != "" {
		prompt += fmt.Sprintf("Product type: %s\n", category)
	}
	mime := img.Mime
	if mime == "" {
		mime = "image/jpeg"
	}
	parts := []mmPart{
		{Type: "text", Text: prompt},
		{Type: "image_url", ImageURL: &mmImageURL{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)}},
	}
	body, _ := json.Marshal(mmReq{
		Model:       visionModel,
		MaxTokens:   30,
		Temperature: 0,
		Messages: []mmMessage{
			{Role: "system", Content: sys},
			{Role: "user", Content: parts},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16384))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, 0, fmt.Errorf("openrouter http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	content, err := extractContent(raw)
	if err != nil {
		return false, 0, err
	}
	m := jsonObj.FindString(content)
	if m == "" {
		return false, 0, fmt.Errorf("no JSON in image-verify reply: %q", content)
	}
	var out struct {
		Match      bool    `json:"match"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(m), &out); err != nil {
		return false, 0, err
	}
	return out.Match, out.Confidence, nil
}
