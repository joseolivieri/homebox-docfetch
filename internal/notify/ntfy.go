// Package notify sends ntfy notifications (review-gate prompts and the weekly
// reconcile digest).
package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Ntfy struct {
	url   string // base, e.g. http://ntfy:8080
	topic string
	http  *http.Client
}

func New(baseURL, topic string) *Ntfy {
	return &Ntfy{
		url:   strings.TrimRight(baseURL, "/"),
		topic: topic,
		http:  &http.Client{Timeout: 15 * time.Second},
	}
}

// Message is a ntfy publish with optional title, click URL, and tags.
type Message struct {
	Title string
	Body  string
	Click string   // URL opened when the notification is tapped
	Tags  []string // ntfy emoji shortcodes / labels
}

// Send publishes to the configured topic. A nil/empty client (no ntfy_url) is a no-op.
func (n *Ntfy) Send(ctx context.Context, m Message) error {
	if n == nil || n.url == "" || n.topic == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url+"/"+n.topic, strings.NewReader(m.Body))
	if err != nil {
		return err
	}
	if m.Title != "" {
		req.Header.Set("Title", m.Title)
	}
	if m.Click != "" {
		req.Header.Set("Click", m.Click)
	}
	if len(m.Tags) > 0 {
		req.Header.Set("Tags", strings.Join(m.Tags, ","))
	}
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ntfy http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
