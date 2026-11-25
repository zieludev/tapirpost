package content

import (
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// FieldType names the kind of value a field holds.
type FieldType string

// The field types a content type can use.
const (
	FieldString   FieldType = "string"   // one line of text
	FieldText     FieldType = "text"     // text of any shape
	FieldInteger  FieldType = "integer"  // a 64-bit integer
	FieldNumber   FieldType = "number"   // a 64-bit float
	FieldBoolean  FieldType = "boolean"  // true or false
	FieldDateTime FieldType = "datetime" // an RFC 3339 timestamp, stored in UTC
	FieldEnum     FieldType = "enum"     // one of Values
	FieldJSON     FieldType = "json"     // any JSON value, not filterable
)

// Limits on a content type definition.
const (
	MaxFields         = 100
	MaxEnumValues     = 100
	MaxDescriptionLen = 1000
	maxPatternLen     = 500
	maxEnumValueLen   = 100
)

// nameRE is deliberately narrow: names end up in URLs, query parameters and
// JSON paths, and nothing in that set needs escaping anywhere.
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

var reservedFieldNames = map[string]bool{
	"id": true, "created_at": true, "updated_at": true, "published_at": true,
}

// Field is one typed field of a content type.
type Field struct {
	Name      string    `json:"name"`
	Type      FieldType `json:"type"`
	Required  bool      `json:"required,omitempty"`
	MinLength *int      `json:"min_length,omitempty"`
	MaxLength *int      `json:"max_length,omitempty"`
	Pattern   string    `json:"pattern,omitempty"`
	Min       *float64  `json:"min,omitempty"`
	Max       *float64  `json:"max,omitempty"`
	Values    []string  `json:"values,omitempty"`
}

// TypeSpec is what a client supplies to create or change a content type.
type TypeSpec struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Fields      []Field `json:"fields"`
}

// Type is a stored content type.
//
// SchemaVersion changes when the fields change. Revision changes whenever
// what the delivery API returns for the type may have changed.
//
// ID tells apart two types that had the same name one after the other. It is
// not part of the API: a name is all a client needs.
type Type struct {
	ID            string    `json:"-"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	Fields        []Field   `json:"fields"`
	SchemaVersion int64     `json:"schema_version"`
	Revision      int64     `json:"revision"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Field returns the field with the given name.
func (t Type) Field(name string) (Field, bool) {
	for _, f := range t.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// ValidName reports whether s can name a content type or a field.
func ValidName(s string) bool {
	return nameRE.MatchString(s)
}

func (s TypeSpec) validate() error {
	var is issues
	if !ValidName(s.Name) {
		is.add("name", "must match %s", nameRE)
	}
	if utf8.RuneCountInString(s.Description) > MaxDescriptionLen {
		is.add("description", "must be at most %d characters", MaxDescriptionLen)
	}
	if len(s.Fields) == 0 {
		is.add("fields", "must contain at least one field")
	}
	if len(s.Fields) > MaxFields {
		is.add("fields", "must contain at most %d fields", MaxFields)
	}
	seen := make(map[string]bool, len(s.Fields))
	for i, f := range s.Fields {
		at := fmt.Sprintf("fields[%d]", i)
		switch {
		case !ValidName(f.Name):
			is.add(at+".name", "must match %s", nameRE)
		case reservedFieldNames[f.Name]:
			is.add(at+".name", "%q is reserved", f.Name)
		case seen[f.Name]:
			is.add(at+".name", "%q is used twice", f.Name)
		}
		seen[f.Name] = true
		f.validate(at, &is)
	}
	return is.err()
}

func (f Field) validate(at string, is *issues) {
	textual := f.Type == FieldString || f.Type == FieldText
	numeric := f.Type == FieldInteger || f.Type == FieldNumber

	switch f.Type {
	case FieldString, FieldText, FieldInteger, FieldNumber, FieldBoolean, FieldDateTime, FieldEnum, FieldJSON:
	default:
		is.add(at+".type", "%q is not a field type", f.Type)
		return
	}

	if !textual && (f.MinLength != nil || f.MaxLength != nil || f.Pattern != "") {
		is.add(at, "min_length, max_length and pattern apply to string and text fields only")
	}
	if !numeric && (f.Min != nil || f.Max != nil) {
		is.add(at, "min and max apply to integer and number fields only")
	}
	if f.Type != FieldEnum && len(f.Values) > 0 {
		is.add(at+".values", "applies to enum fields only")
	}

	if f.MinLength != nil && *f.MinLength < 0 {
		is.add(at+".min_length", "must not be negative")
	}
	if f.MaxLength != nil && *f.MaxLength < 1 {
		is.add(at+".max_length", "must be at least 1")
	}
	if f.MinLength != nil && f.MaxLength != nil && *f.MinLength > *f.MaxLength {
		is.add(at+".min_length", "must not exceed max_length")
	}
	if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
		is.add(at+".min", "must not exceed max")
	}
	if f.Pattern != "" {
		if len(f.Pattern) > maxPatternLen {
			is.add(at+".pattern", "must be at most %d bytes", maxPatternLen)
		} else if _, err := compilePattern(f.Pattern); err != nil {
			is.add(at+".pattern", "is not a valid regular expression: %v", err)
		}
	}

	if f.Type == FieldEnum {
		if len(f.Values) == 0 {
			is.add(at+".values", "must list at least one value")
		}
		if len(f.Values) > MaxEnumValues {
			is.add(at+".values", "must list at most %d values", MaxEnumValues)
		}
		seen := make(map[string]bool, len(f.Values))
		for _, v := range f.Values {
			if v == "" || len(v) > maxEnumValueLen {
				is.add(at+".values", "values must be 1 to %d bytes long", maxEnumValueLen)
			}
			if seen[v] {
				is.add(at+".values", "%q is listed twice", v)
			}
			seen[v] = true
		}
	}
}

// removedOrRetyped lists the fields of old that are gone in next, and reports
// a field whose type changed as an error: its stored values would no longer
// match the type, so the client has to remove it and add it back.
func removedOrRetyped(old, next []Field) ([]string, error) {
	nextByName := make(map[string]Field, len(next))
	for _, f := range next {
		nextByName[f.Name] = f
	}
	var removed []string
	var is issues
	for _, f := range old {
		n, ok := nextByName[f.Name]
		switch {
		case !ok:
			removed = append(removed, f.Name)
		case n.Type != f.Type:
			is.add("fields", "the type of %q cannot change from %s to %s; remove the field and add it again", f.Name, f.Type, n.Type)
		}
	}
	return removed, is.err()
}
