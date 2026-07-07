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

// IntakeImage is one photo submitted at portal intake.
type IntakeImage struct {
	Data []byte
	Mime string // image/jpeg, image/png, image/webp
}

// IntakeExtraction is the structured result of reading 1-2 intake photos.
// Any block the model could not see is left zero-valued.
type IntakeExtraction struct {
	Sticker struct {
		Manufacturer string `json:"manufacturer"`
		ModelNumber  string `json:"modelNumber"`
		SerialNumber string `json:"serialNumber"`
		ProductType  string `json:"productType"`
	} `json:"sticker"`
	Receipt struct {
		PurchaseFrom  string  `json:"purchaseFrom"`
		PurchaseDate  string  `json:"purchaseDate"` // YYYY-MM-DD
		PurchasePrice float64 `json:"purchasePrice"`
		NameHint      string  `json:"nameHint"`
	} `json:"receipt"`
	Name       string             `json:"name"` // best canonical product name
	Confidence map[string]float64 `json:"confidence"`
}

// multimodal chat request types (OpenAI-compatible content-array form).
type mmPart struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ImageURL *mmImageURL `json:"image_url,omitempty"`
}
type mmImageURL struct {
	URL string `json:"url"`
}
type mmMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string (system) or []mmPart (user)
}
type mmReq struct {
	Model       string      `json:"model"`
	Messages    []mmMessage `json:"messages"`
	MaxTokens   int         `json:"max_tokens"`
	Temperature float64     `json:"temperature"`
}

// ExtractIntake reads model-label and/or receipt photos with the configured
// vision model and returns structured identity + purchase fields. User-initiated
// (portal), so volume is low; images are the dominant token cost (~1-1.5k each).
func (c *Client) ExtractIntake(ctx context.Context, visionModel string, images []IntakeImage) (*IntakeExtraction, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("no images provided")
	}
	if visionModel == "" {
		return nil, fmt.Errorf("no vision model configured")
	}

	sys := "You read photos of product model/serial stickers and purchase receipts. Classify each " +
		"image yourself. Extract only what is visible; empty string/0 for anything not present. " +
		"modelNumber is the manufacturer's model/part number, never a marketing phrase. " +
		"purchaseDate must be YYYY-MM-DD. name is the best canonical product name you can infer. " +
		`Respond ONLY with JSON: {"sticker":{"manufacturer":"","modelNumber":"","serialNumber":"","productType":""},` +
		`"receipt":{"purchaseFrom":"","purchaseDate":"","purchasePrice":0,"nameHint":""},` +
		`"name":"","confidence":{"manufacturer":0,"modelNumber":0,"serialNumber":0,"name":0,"purchase":0}}. No prose.`

	parts := []mmPart{{Type: "text", Text: "Extract product identity and purchase info from these photos."}}
	for _, img := range images {
		mime := img.Mime
		if mime == "" {
			mime = "image/jpeg"
		}
		parts = append(parts, mmPart{
			Type:     "image_url",
			ImageURL: &mmImageURL{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)},
		})
	}

	body, _ := json.Marshal(mmReq{
		Model:       visionModel,
		MaxTokens:   300,
		Temperature: 0,
		Messages: []mmMessage{
			{Role: "system", Content: sys},
			{Role: "user", Content: parts},
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32768))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openrouter http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	content, err := extractContent(raw)
	if err != nil {
		return nil, err
	}
	m := jsonBlock.FindString(content)
	if m == "" {
		return nil, fmt.Errorf("no JSON in vision reply: %q", content)
	}
	var out IntakeExtraction
	if err := json.Unmarshal([]byte(m), &out); err != nil {
		return nil, fmt.Errorf("parse intake extraction %q: %w", m, err)
	}
	if out.Confidence == nil {
		out.Confidence = map[string]float64{}
	}
	return &out, nil
}
