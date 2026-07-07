package homebox

import "time"

// EntityType — /v1/entity-types. Locations have IsLocation=true. This fork has
// no dedicated "Item" type; items are entities created with no entityTypeId.
type EntityType struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsLocation bool   `json:"isLocation"`
	Icon       string `json:"icon"`
}

// Tag — this fork's replacement for labels. /v1/tags.
type Tag struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ParentID string `json:"parentId,omitempty"`
	Color    string `json:"color,omitempty"`
	Icon     string `json:"icon,omitempty"`
}

type TagCreate struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
	Icon        string `json:"icon,omitempty"`
	ParentID    string `json:"parentId,omitempty"`
}

// EntitySummary — item shape returned by list (/v1/entities .items[]).
type EntitySummary struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	EntityType  EntityType `json:"entityType"`
	Tags        []Tag      `json:"tags"`
	AssetID     string     `json:"assetId"`
	ImageID     string     `json:"imageId"`
	ThumbnailID string     `json:"thumbnailId"`
	Quantity    float64    `json:"quantity"`
	Archived    bool       `json:"archived"`
	ItemCount   int        `json:"itemCount"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// EntityListResult — paginated list wrapper. Results live under Items.
type EntityListResult struct {
	Page     int             `json:"page"`
	PageSize int             `json:"pageSize"`
	Total    int             `json:"total"`
	Items    []EntitySummary `json:"items"`
}

// Attachment — sub-object of EntityOut.attachments[].
type Attachment struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Title   string `json:"title"`
	Primary bool   `json:"primary"`
}

// EntityOut — full entity detail (/v1/entities/{id}). Carries every field a
// PUT full-replace must round-trip (see scheduler.fullUpdateFrom) so machine
// writes never blank existing data.
type EntityOut struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	Description      string       `json:"description"`
	EntityType       EntityType   `json:"entityType"`
	Tags             []Tag        `json:"tags"`
	Attachments      []Attachment `json:"attachments"`
	AssetID          string       `json:"assetId"`
	Manufacturer     string       `json:"manufacturer"`
	ModelNumber      string       `json:"modelNumber"`
	SerialNumber     string       `json:"serialNumber"`
	Notes            string       `json:"notes"`
	ImageID          string       `json:"imageId"`
	Quantity         float64      `json:"quantity"`
	Insured          bool         `json:"insured"`
	Archived         bool         `json:"archived"`
	LifetimeWarranty bool         `json:"lifetimeWarranty"`
	PurchaseFrom     string       `json:"purchaseFrom"`
	PurchaseDate     string       `json:"purchaseDate"`
	PurchasePrice    float64      `json:"purchasePrice"`
	WarrantyExpires  string       `json:"warrantyExpires"`
	WarrantyDetails  string       `json:"warrantyDetails"`
	UpdatedAt        time.Time    `json:"updatedAt"`
}

// EntityCreate — POST /v1/entities. Only Name is required. No location field.
// EntityTypeID is omitted for items (no "Item" type exists).
type EntityCreate struct {
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	EntityTypeID string   `json:"entityTypeId,omitempty"`
	ParentID     string   `json:"parentId,omitempty"`
	TagIDs       []string `json:"tagIds,omitempty"`
	Quantity     float64  `json:"quantity,omitempty"`
}

// EntityUpdate — PATCH /v1/entities/{id}. Name required by the API. Pointer
// fields let callers send only what changed; nil is omitted.
type EntityUpdate struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Manufacturer     *string  `json:"manufacturer,omitempty"`
	ModelNumber      *string  `json:"modelNumber,omitempty"`
	SerialNumber     *string  `json:"serialNumber,omitempty"`
	AssetID          *string  `json:"assetId,omitempty"`
	Notes            *string  `json:"notes,omitempty"`
	Description      *string  `json:"description,omitempty"`
	Quantity         *float64 `json:"quantity,omitempty"`
	Insured          *bool    `json:"insured,omitempty"`
	Archived         *bool    `json:"archived,omitempty"`
	LifetimeWarranty *bool    `json:"lifetimeWarranty,omitempty"`
	PurchaseFrom     *string  `json:"purchaseFrom,omitempty"`
	PurchaseDate     *string  `json:"purchaseDate,omitempty"`
	PurchasePrice    *float64 `json:"purchasePrice,omitempty"`
	WarrantyExpires  *string  `json:"warrantyExpires,omitempty"`
	WarrantyDetails  *string  `json:"warrantyDetails,omitempty"`
	ParentID         *string  `json:"parentId,omitempty"`
	TagIDs           []string `json:"tagIds,omitempty"`
}
