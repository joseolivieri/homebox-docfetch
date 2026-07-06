package homebox

import "context"

// EnsureTag returns the id of the tag with the given name, creating it if it
// does not exist. Idempotent — safe to call every startup. (P1-03)
func (c *Client) EnsureTag(ctx context.Context, name string) (string, error) {
	tags, err := c.ListTags(ctx)
	if err != nil {
		return "", err
	}
	for _, t := range tags {
		if t.Name == name {
			return t.ID, nil
		}
	}
	created, err := c.CreateTag(ctx, TagCreate{Name: name})
	if err != nil {
		return "", err
	}
	return created.ID, nil
}
