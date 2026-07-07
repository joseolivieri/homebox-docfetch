// Package homebox is a typed REST client for the Homebox entity-model fork.
//
// Classic Homebox concepts do not apply here: there is no /items, no /labels,
// and no locationId. Items are "entities" (created with no entityTypeId),
// labels are "tags", and an item's location is its parent entity. See
// docs/spec.md §6 for the verified API surface.
package homebox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	base  string // ".../api/v1"
	token string
	http  *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		base:  strings.TrimRight(baseURL, "/") + "/api/v1",
		token: token,
		http:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.http.Do(req)
}

// decodeJSON runs a JSON request and decodes a JSON response into out (may be nil).
func (c *Client) json(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.do(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s: http %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// --- Entity types ---

func (c *Client) ListEntityTypes(ctx context.Context) ([]EntityType, error) {
	var out []EntityType
	return out, c.json(ctx, http.MethodGet, "/entity-types", nil, &out)
}

// --- Tags ---

func (c *Client) ListTags(ctx context.Context) ([]Tag, error) {
	var out []Tag
	return out, c.json(ctx, http.MethodGet, "/tags", nil, &out)
}

func (c *Client) CreateTag(ctx context.Context, in TagCreate) (*Tag, error) {
	var out Tag
	if err := c.json(ctx, http.MethodPost, "/tags", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Entities ---

// ListEntities returns one page. tagIDs filters by tag (AND-less; server semantics).
func (c *Client) ListEntities(ctx context.Context, page, pageSize int, tagIDs []string) (*EntityListResult, error) {
	q := url.Values{}
	q.Set("page", strconv.Itoa(page))
	q.Set("pageSize", strconv.Itoa(pageSize))
	for _, t := range tagIDs {
		q.Add("tags", t)
	}
	var out EntityListResult
	return &out, c.json(ctx, http.MethodGet, "/entities?"+q.Encode(), nil, &out)
}

func (c *Client) GetEntity(ctx context.Context, id string) (*EntityOut, error) {
	var out EntityOut
	if err := c.json(ctx, http.MethodGet, "/entities/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateEntity(ctx context.Context, in EntityCreate) (*EntityOut, error) {
	var out EntityOut
	if err := c.json(ctx, http.MethodPost, "/entities", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PatchEntity partially updates an entity. NOTE (verified live): this fork's
// PATCH honors tagIds/archived and preserves other fields, but SILENTLY IGNORES
// scalar metadata (manufacturer, modelNumber, serialNumber, purchase*, …).
// Use it for tag changes only. For metadata writes use PutEntity.
func (c *Client) PatchEntity(ctx context.Context, id string, in EntityUpdate) (*EntityOut, error) {
	var out EntityOut
	if err := c.json(ctx, http.MethodPatch, "/entities/"+id, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PutEntity fully updates an entity (PUT). Required for scalar metadata
// (manufacturer/modelNumber/purchase/warranty) which PATCH ignores. Because it
// is a full replace, callers must send the complete desired field set (fetch
// via GetEntity first and merge) to avoid blanking existing data.
func (c *Client) PutEntity(ctx context.Context, id string, in EntityUpdate) (*EntityOut, error) {
	var out EntityOut
	if err := c.json(ctx, http.MethodPut, "/entities/"+id, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteEntity(ctx context.Context, id string) error {
	return c.json(ctx, http.MethodDelete, "/entities/"+id, nil, nil)
}

// --- Attachments ---

// UploadAttachment posts a file as multipart form data. attType is one of the
// Homebox enum values (manual, photo, receipt, warranty, attachment). primary
// marks the main image (photos). Returns the updated entity.
func (c *Client) UploadAttachment(ctx context.Context, entityID, filename, attType string, primary bool, r io.Reader) (*EntityOut, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(fw, r); err != nil {
		return nil, err
	}
	_ = mw.WriteField("name", filename)
	if attType != "" {
		_ = mw.WriteField("type", attType)
	}
	if primary {
		_ = mw.WriteField("primary", "true")
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	resp, err := c.do(ctx, http.MethodPost, "/entities/"+entityID+"/attachments", &buf, mw.FormDataContentType())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("upload attachment: http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out EntityOut
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		// Some Homebox builds return 204/empty on upload; treat decode EOF as success.
		if err == io.EOF {
			return &out, nil
		}
		return nil, err
	}
	return &out, nil
}
