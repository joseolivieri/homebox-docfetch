package discovery

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Download fetches a URL body, capped at maxBytes. Returns an error if the
// response exceeds the cap or is not 2xx. Used to pull a manual PDF before
// hashing + attaching.
func (e *Engine) Download(ctx context.Context, url string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download %s: http %d", url, resp.StatusCode)
	}
	// +1 so we can detect an over-cap body.
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("download %s: exceeds cap of %d bytes", url, maxBytes)
	}
	return b, nil
}
