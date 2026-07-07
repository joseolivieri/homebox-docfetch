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

var domainRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*\.[a-z]{2,}$`)

// BrandDomain returns the manufacturer's official website domain (e.g.
// "anker.com"), or "" when the model is unsure. Callers cache the answer —
// one call per manufacturer per process.
func (c *Client) BrandDomain(ctx context.Context, manufacturer string) (string, error) {
	sys := "You know consumer product brands. Return the brand's OFFICIAL website domain " +
		"(bare domain, no scheme, no www). Empty string if you are not sure. " +
		`Respond ONLY with JSON: {"domain":""}. No prose.`

	reqBody, _ := json.Marshal(chatReq{
		Model:       c.rerankModel,
		MaxTokens:   30,
		Temperature: 0,
		Messages: []message{
			{Role: "system", Content: sys},
			{Role: "user", Content: "Brand: " + manufacturer},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("openrouter http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	content, err := extractContent(raw)
	if err != nil {
		return "", err
	}
	m := jsonObj.FindString(content)
	if m == "" {
		return "", nil
	}
	var out struct {
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal([]byte(m), &out); err != nil {
		return "", nil
	}
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(out.Domain)), "www.")
	if !domainRe.MatchString(d) {
		return "", nil
	}
	return d, nil
}
