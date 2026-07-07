package homebox

// UpsertField sets a text custom field by name, replacing an existing value.
func UpsertField(fields []EntityField, name, value string) []EntityField {
	for i := range fields {
		if fields[i].Name == name {
			fields[i].TextValue = value
			fields[i].Type = "text"
			return fields
		}
	}
	return append(fields, EntityField{Name: name, Type: "text", TextValue: value})
}

// FieldValue returns the text value of a named field ("" when absent).
func FieldValue(fields []EntityField, name string) string {
	for i := range fields {
		if fields[i].Name == name {
			return fields[i].TextValue
		}
	}
	return ""
}
