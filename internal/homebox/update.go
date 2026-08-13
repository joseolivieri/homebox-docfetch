package homebox

// FullUpdateFrom builds a complete EntityUpdate mirroring the current entity,
// so a PUT (full replace) preserves everything the caller is not changing.
// Hard-won invariants (violations caused real data loss, see CLAUDE.md):
// omit Fields and every custom field is wiped; omit ParentID and the location
// is cleared; PATCH silently drops scalar changes, so all metadata writes go
// through PUT built from this.
func FullUpdateFrom(d *EntityOut) EntityUpdate {
	upd := EntityUpdate{ID: d.ID, Name: d.Name}
	cp := func(v string) *string { s := v; return &s }
	upd.Manufacturer = cp(d.Manufacturer)
	upd.ModelNumber = cp(d.ModelNumber)
	upd.SerialNumber = cp(d.SerialNumber)
	upd.AssetID = cp(d.AssetID)
	upd.Notes = cp(d.Notes)
	upd.Description = cp(d.Description)
	upd.Quantity = &d.Quantity
	upd.Insured = &d.Insured
	upd.Archived = &d.Archived
	upd.LifetimeWarranty = &d.LifetimeWarranty
	upd.PurchaseFrom = cp(d.PurchaseFrom)
	upd.PurchaseDate = cp(d.PurchaseDate)
	upd.PurchasePrice = &d.PurchasePrice
	upd.WarrantyExpires = cp(d.WarrantyExpires)
	upd.WarrantyDetails = cp(d.WarrantyDetails)
	if d.Parent != nil && d.Parent.ID != "" {
		// PUT is a full replace; omitting parentId clears the location.
		upd.ParentID = cp(d.Parent.ID)
	}
	// PUT without fields wipes all custom fields (verified live) — round-trip.
	upd.Fields = d.Fields
	var tags []string
	for _, t := range d.Tags {
		tags = append(tags, t.ID)
	}
	upd.TagIDs = tags
	return upd
}
