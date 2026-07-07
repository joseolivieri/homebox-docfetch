package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Identity is the structured extraction result for one item. Confidence values
// are the model's self-scores — callers MUST corroborate before writing
// (see enrich: domain agreement + back-check); these scores alone never
// authorize a write.
type Identity struct {
	Manufacturer string             `json:"manufacturer"`
	ModelNumber  string             `json:"modelNumber"`
	Name         string             `json:"name"`
	Category     string             `json:"category"`
	Confidence   map[string]float64 `json:"confidence"`
}

var jsonBlock = regexp.MustCompile(`(?s)\{.*\}`)

// ExtractIdentity infers the full product identity from search-result
// candidates. Same cost discipline as Rerank: the model sees only short
// title/url/snippet triples and returns constrained JSON.
func (c *Client) ExtractIdentity(ctx context.Context, itemDesc string, cands []Candidate) (*Identity, error) {
	if len(cands) == 0 {
		return nil, fmt.Errorf("no candidates to extract from")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Item as entered by user: %s\nSearch results:\n", itemDesc)
	for i, cd := range cands {
		fmt.Fprintf(&b, "[%d] %s | %s | %s\n", i, cd.Title, cd.URL, cd.Snippet)
	}
	sys := "You identify consumer products. From the search results, infer the product's canonical " +
		"identity. modelNumber must be the manufacturer's model/part number (e.g. CFI-Y1000, WH-1000XM5), " +
		"never a marketing phrase; empty string if not evident. category is a short lowercase noun phrase. " +
		`Respond ONLY with JSON: {"manufacturer":"","modelNumber":"","name":"","category":"",` +
		`"confidence":{"manufacturer":0.0,"modelNumber":0.0,"name":0.0,"category":0.0}}. No prose.`

	reqBody, _ := json.Marshal(chatReq{
		Model:       c.rerankModel,
		MaxTokens:   160,
		Temperature: 0,
		Messages: []message{
			{Role: "system", Content: sys},
			{Role: "user", Content: b.String()},
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(reqBody))
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16384))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openrouter http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	content, err := extractContent(raw)
	if err != nil {
		return nil, err
	}
	m := jsonBlock.FindString(content)
	if m == "" {
		return nil, fmt.Errorf("no JSON in model reply: %q", content)
	}
	var id Identity
	if err := json.Unmarshal([]byte(m), &id); err != nil {
		return nil, fmt.Errorf("parse identity %q: %w", m, err)
	}
	if id.Confidence == nil {
		id.Confidence = map[string]float64{}
	}
	return &id, nil
}
