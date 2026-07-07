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

// VerifyDoc checks whether a downloaded document's opening text is actually a
// manual/guide for the given item. This is the content-level guard the search
// layer can't provide: listing-hosted PDFs (Amazon quick-view etc.) match the
// item in the search snippet while containing a different product's manual.
func (c *Client) VerifyDoc(ctx context.Context, itemDesc, excerpt string) (match bool, confidence float64, err error) {
	if strings.TrimSpace(excerpt) == "" {
		return false, 0, fmt.Errorf("empty excerpt")
	}
	sys := "You verify that a document belongs to a product. Given the product identity and the " +
		"opening text of a downloaded document, decide if the document is a manual/guide/datasheet " +
		"FOR THAT EXACT PRODUCT. A manual for a different model or a sibling brand of the same " +
		"company (e.g. Soundcore vs Anker) is NOT a match. If the document text identifies a model " +
		"number that differs from the product's model number, match=false even when the product " +
		"family or brand matches. " +
		`Respond ONLY with JSON: {"match":false,"confidence":0.0}. No prose.`
	user := fmt.Sprintf("Product: %s\n\nDocument opening text:\n%s", itemDesc, excerpt)

	reqBody, _ := json.Marshal(chatReq{
		Model:       c.rerankModel,
		MaxTokens:   30,
		Temperature: 0,
		Messages: []message{
			{Role: "system", Content: sys},
			{Role: "user", Content: user},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(reqBody))
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, 0, fmt.Errorf("openrouter http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	content, err := extractContent(raw)
	if err != nil {
		return false, 0, err
	}
	m := jsonObj.FindString(content)
	if m == "" {
		return false, 0, fmt.Errorf("no JSON in verify reply: %q", content)
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
