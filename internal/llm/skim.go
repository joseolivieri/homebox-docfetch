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

// DocSkim is the structured read of a document's opening text: what kind of
// doc it is, whose product it covers, and which model numbers it declares.
// Replaces the old boolean VerifyDoc — one call now serves the attach veto
// (DifferentProduct / wrong DocType) AND the promote path (Models confirm an
// item whose model number never appears in the URL).
type DocSkim struct {
	DocType          string   `json:"docType"` // manual|parts|quickstart|datasheet|warranty|other
	Manufacturer     string   `json:"manufacturer"`
	Models           []string `json:"models"`
	DifferentProduct bool     `json:"differentProduct"`
	Confidence       float64  `json:"confidence"`
}

// SkimDoc extracts structured identity facts from a document's opening text.
// Cheap-by-design: sees only a ~1.5KB excerpt, constrained JSON out.
func (c *Client) SkimDoc(ctx context.Context, itemDesc, wantClass, excerpt string) (*DocSkim, error) {
	if strings.TrimSpace(excerpt) == "" {
		return nil, fmt.Errorf("empty excerpt")
	}
	sys := "You skim the opening text of a downloaded product document and extract facts. " +
		"docType is one of: manual, parts, quickstart, datasheet, warranty, other — pick 'parts' for " +
		"parts lists/diagrams, 'manual' for user/owner/use-and-care guides. models lists the exact " +
		"model/part numbers the document declares it covers (empty if none are stated). " +
		"differentProduct=true ONLY when the text positively identifies a DIFFERENT product than the " +
		"given one (a different model line or sibling brand — e.g. a Soundcore speaker doc for an " +
		"Anker charger). Generic safety/usage text without product identifiers: differentProduct=false. " +
		`Respond ONLY with JSON: {"docType":"","manufacturer":"","models":[],"differentProduct":false,"confidence":0.0}. No prose.`
	user := fmt.Sprintf("Product: %s\nExpected document kind: %s\n\nDocument opening text:\n%s", itemDesc, wantClass, excerpt)

	reqBody, _ := json.Marshal(chatReq{
		Model:       c.rerankModel,
		MaxTokens:   140,
		Temperature: 0,
		Messages: []message{
			{Role: "system", Content: sys},
			{Role: "user", Content: user},
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
	m := jsonObj.FindString(content)
	if m == "" {
		return nil, fmt.Errorf("no JSON in skim reply: %q", content)
	}
	var out DocSkim
	if err := json.Unmarshal([]byte(m), &out); err != nil {
		return nil, err
	}
	out.DocType = strings.ToLower(strings.TrimSpace(out.DocType))
	return &out, nil
}
