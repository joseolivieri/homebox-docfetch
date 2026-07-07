package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// WarrantyEstimate is the model's read of a manufacturer's warranty term from
// search snippets. Months==0 means unknown.
type WarrantyEstimate struct {
	Months     int     `json:"months"`
	Lifetime   bool    `json:"lifetime"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"`
	ClaimsURL  string  `json:"claimsUrl"`
}

// EstimateWarranty infers the standard manufacturer warranty term for a product
// from search-result snippets. Text-only, cheap (rerank-class call).
func (c *Client) EstimateWarranty(ctx context.Context, itemDesc string, cands []Candidate) (*WarrantyEstimate, error) {
	if len(cands) == 0 {
		return &WarrantyEstimate{}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Product: %s\nSearch results about its warranty:\n", itemDesc)
	for i, cd := range cands {
		fmt.Fprintf(&b, "[%d] %s | %s | %s\n", i, cd.Title, cd.URL, cd.Snippet)
	}
	sys := "You determine the STANDARD manufacturer warranty term for a product from search snippets. " +
		"Only report a term the snippets actually state for this product or its manufacturer's standard " +
		"policy; months=0 if unclear. lifetime=true only when the terms explicitly say lifetime warranty. " +
		"source is the supporting URL. claimsUrl is the manufacturer's " +
		"warranty-claims/registration/support page if one appears in the results, else empty. " +
		`Respond ONLY with JSON: {"months":0,"lifetime":false,"confidence":0.0,"source":"","claimsUrl":""}. No prose.`

	reqBody, _ := json.Marshal(chatReq{
		Model:       c.rerankModel,
		MaxTokens:   60,
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openrouter http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	content, err := extractContent(raw)
	if err != nil {
		return nil, err
	}
	m := jsonBlock.FindString(content)
	if m == "" {
		return nil, fmt.Errorf("no JSON in warranty reply: %q", content)
	}
	var out WarrantyEstimate
	if err := json.Unmarshal([]byte(m), &out); err != nil {
		return nil, err
	}
	if out.Months < 0 || out.Months > 360 {
		out.Months = 0
	}
	return &out, nil
}
