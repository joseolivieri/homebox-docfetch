// Package llm is a thin OpenRouter (OpenAI-compatible) client. It is
// modality-agnostic: Phase 1 uses text-only Rerank; Phase 2 adds a vision
// call (extractor.go) on the same Client without structural change.
//
// Cost discipline (see docs/spec.md §7): the model never sees document
// contents, only short title/url/snippet triples, and returns a tiny
// constrained JSON object.
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
	"time"
)

type Client struct {
	base        string
	key         string
	rerankModel string
	http        *http.Client
}

func New(baseURL, apiKey, rerankModel string) *Client {
	return &Client{
		base:        strings.TrimRight(baseURL, "/"),
		key:         apiKey,
		rerankModel: rerankModel,
		http:        &http.Client{Timeout: 45 * time.Second},
	}
}

// Candidate is the minimal view of a search result the model is allowed to see.
type Candidate struct {
	Title   string
	URL     string
	Snippet string
}

// chat request/response (OpenAI-compatible subset).
type chatReq struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
}
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

var jsonObj = regexp.MustCompile(`\{[^{}]*\}`)

// Rerank asks the model to pick the single best manual/support doc among the
// candidates for the given item. Returns a zero-based index (or -1 if none fit)
// and a confidence in [0,1]. Snippets are truncated by the caller.
func (c *Client) Rerank(ctx context.Context, itemDesc string, cands []Candidate) (bestIdx int, confidence float64, err error) {
	if len(cands) == 0 {
		return -1, 0, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Item: %s\nCandidates:\n", itemDesc)
	for i, cd := range cands {
		fmt.Fprintf(&b, "[%d] %s | %s | %s\n", i, cd.Title, cd.URL, cd.Snippet)
	}
	sys := "You select the single best OFFICIAL user manual or support document for a product " +
		"from search candidates. Prefer manufacturer/official domains and PDFs matching the exact model. " +
		"A document for a DIFFERENT model, or for a sibling/sub-brand of the same company, is WRONG — " +
		"when no candidate clearly matches the exact product, return best=-1 rather than guessing. " +
		`Respond with ONLY JSON: {"best":<index or -1 if none>,"confidence":<0.0-1.0>}. No prose.`

	reqBody, _ := json.Marshal(chatReq{
		Model:       c.rerankModel,
		MaxTokens:   40,
		Temperature: 0,
		Messages: []message{
			{Role: "system", Content: sys},
			{Role: "user", Content: b.String()},
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(reqBody))
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return -1, 0, fmt.Errorf("openrouter http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	content, err := extractContent(raw)
	if err != nil {
		return -1, 0, err
	}
	var out struct {
		Best       int     `json:"best"`
		Confidence float64 `json:"confidence"`
	}
	m := jsonObj.FindString(content)
	if m == "" {
		return -1, 0, fmt.Errorf("no JSON in model reply: %q", content)
	}
	if err := json.Unmarshal([]byte(m), &out); err != nil {
		return -1, 0, fmt.Errorf("parse model reply %q: %w", m, err)
	}
	if out.Best < -1 || out.Best >= len(cands) {
		out.Best = -1
	}
	if out.Confidence < 0 {
		out.Confidence = 0
	} else if out.Confidence > 1 {
		out.Confidence = 1
	}
	return out.Best, out.Confidence, nil
}

// extractContent pulls choices[0].message.content out of an OpenAI-style reply.
func extractContent(raw []byte) (string, error) {
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("decode chat response: %w", err)
	}
	if r.Error != nil {
		return "", fmt.Errorf("openrouter error: %s", r.Error.Message)
	}
	if len(r.Choices) == 0 {
		return "", fmt.Errorf("empty choices")
	}
	return r.Choices[0].Message.Content, nil
}
