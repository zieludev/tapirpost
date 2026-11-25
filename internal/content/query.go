package content

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Op is a filter operator.
type Op string

// The filter operators.
const (
	OpEq       Op = "eq"
	OpNe       Op = "ne"
	OpLt       Op = "lt"
	OpLte      Op = "lte"
	OpGt       Op = "gt"
	OpGte      Op = "gte"
	OpIn       Op = "in"
	OpContains Op = "contains"
)

// Limits on a listing.
const (
	DefaultLimit = 20
	MaxLimit     = 100
	MaxFilters   = 10
	MaxSortKeys  = 3
	MaxInValues  = 50
)

// Kind tells a store how the values of a field compare.
type Kind int

// The comparison kinds.
const (
	KindString  Kind = iota // UTF-8 byte order
	KindInteger             // int64
	KindNumber              // float64
	KindBoolean             // false before true
	KindTime                // a system timestamp, compared as a moment
)

// FieldRef points at something an entry can be filtered or sorted by: one of
// its own fields, or a system column when System is true.
type FieldRef struct {
	Name   string
	System bool
	Kind   Kind
}

// Filter keeps the entries whose field relates to the values as Op says.
// Values holds one value, or up to MaxInValues for OpIn. Their Go type
// follows Field.Kind: string, int64, float64, bool or time.Time.
type Filter struct {
	Field  FieldRef
	Op     Op
	Values []any
}

// Sort orders entries by a field. Entries without a value come last.
type Sort struct {
	Field FieldRef
	Desc  bool
}

// Query selects, orders and pages entries. Without Sort the newest entries
// come first.
type Query struct {
	Filters []Filter
	Sort    []Sort
	Limit   int
	Offset  int
}

// RawFilter is a filter as a client wrote it, before it is checked against a
// content type.
type RawFilter struct {
	Field string
	Op    string
	Value string
}

// QueryParams is a listing request as a client wrote it. Sort keys may start
// with "-" for descending order. A nil Limit means DefaultLimit.
type QueryParams struct {
	Filters []RawFilter
	Sort    []string
	Limit   *int
	Offset  *int
}

// The system columns. Only the ones that change together with the published
// copy may be used on published content: anything else would let a cached
// listing go stale without the type's revision moving.
var (
	draftSystemFields = map[string]Kind{
		"id": KindString, "created_at": KindTime, "updated_at": KindTime, "published_at": KindTime,
	}
	publishedSystemFields = map[string]Kind{
		"id": KindString, "created_at": KindTime, "published_at": KindTime,
	}
)

var opsByKind = map[Kind]map[Op]bool{
	KindString:  {OpEq: true, OpNe: true, OpLt: true, OpLte: true, OpGt: true, OpGte: true, OpIn: true, OpContains: true},
	KindInteger: {OpEq: true, OpNe: true, OpLt: true, OpLte: true, OpGt: true, OpGte: true, OpIn: true},
	KindNumber:  {OpEq: true, OpNe: true, OpLt: true, OpLte: true, OpGt: true, OpGte: true, OpIn: true},
	KindBoolean: {OpEq: true, OpNe: true},
	KindTime:    {OpEq: true, OpNe: true, OpLt: true, OpLte: true, OpGt: true, OpGte: true},
}

// Query checks a listing request against the type. published selects the
// rules for published content.
func (t Type) Query(p QueryParams, published bool) (Query, error) {
	var is issues
	q := Query{Limit: DefaultLimit}

	if p.Limit != nil {
		q.Limit = *p.Limit
		if q.Limit < 1 || q.Limit > MaxLimit {
			is.add("limit", "must be between 1 and %d", MaxLimit)
		}
	}
	if p.Offset != nil {
		q.Offset = *p.Offset
		if q.Offset < 0 {
			is.add("offset", "must not be negative")
		}
	}

	if len(p.Filters) > MaxFilters {
		is.add("filter", "at most %d filters are allowed", MaxFilters)
	}
	for _, raw := range p.Filters {
		at := "filter[" + raw.Field + "]"
		ref, isTime, ok := t.resolve(raw.Field, published)
		if !ok {
			is.add(at, "cannot be filtered on")
			continue
		}
		op := Op(raw.Op)
		if !opsByKind[ref.Kind][op] || (isTime && op == OpContains) {
			is.add(at, "does not support the operator %q", raw.Op)
			continue
		}
		parts := []string{raw.Value}
		if op == OpIn {
			parts = strings.Split(raw.Value, ",")
			if len(parts) > MaxInValues {
				is.add(at, "at most %d values are allowed", MaxInValues)
				continue
			}
		}
		values := make([]any, 0, len(parts))
		for _, part := range parts {
			v, problem := parseValue(ref.Kind, isTime, part)
			if problem != "" {
				is.add(at, "%s", problem)
				continue
			}
			values = append(values, v)
		}
		if len(values) == len(parts) {
			q.Filters = append(q.Filters, Filter{Field: ref, Op: op, Values: values})
		}
	}

	if len(p.Sort) > MaxSortKeys {
		is.add("sort", "at most %d sort keys are allowed", MaxSortKeys)
	}
	seen := make(map[string]bool, len(p.Sort))
	for _, key := range p.Sort {
		name, desc := strings.CutPrefix(key, "-")
		ref, _, ok := t.resolve(name, published)
		if !ok {
			is.add("sort", "%q cannot be sorted on", name)
			continue
		}
		if seen[name] {
			is.add("sort", "%q is listed twice", name)
			continue
		}
		seen[name] = true
		q.Sort = append(q.Sort, Sort{Field: ref, Desc: desc})
	}

	if err := is.err(); err != nil {
		return Query{}, err
	}
	return q, nil
}

// resolve maps a name from a query to something a store can compare. isTime
// is true for datetime fields, which are stored as fixed-width strings and
// therefore compare as KindString.
func (t Type) resolve(name string, published bool) (ref FieldRef, isTime, ok bool) {
	system := draftSystemFields
	if published {
		system = publishedSystemFields
	}
	if kind, found := system[name]; found {
		return FieldRef{Name: name, System: true, Kind: kind}, false, true
	}
	f, found := t.Field(name)
	if !found {
		return FieldRef{}, false, false
	}
	switch f.Type {
	case FieldString, FieldText, FieldEnum:
		return FieldRef{Name: name, Kind: KindString}, false, true
	case FieldDateTime:
		return FieldRef{Name: name, Kind: KindString}, true, true
	case FieldInteger:
		return FieldRef{Name: name, Kind: KindInteger}, false, true
	case FieldNumber:
		return FieldRef{Name: name, Kind: KindNumber}, false, true
	case FieldBoolean:
		return FieldRef{Name: name, Kind: KindBoolean}, false, true
	}
	return FieldRef{}, false, false
}

func parseValue(kind Kind, isTime bool, s string) (v any, problem string) {
	switch kind {
	case KindString:
		if !isTime {
			return s, ""
		}
		norm, err := normalizeTime(s)
		if err != nil {
			return nil, "must be an RFC 3339 timestamp"
		}
		return norm, ""
	case KindInteger:
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, "must be an integer"
		}
		return i, ""
	case KindNumber:
		x, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, "must be a number"
		}
		return x, ""
	case KindBoolean:
		switch s {
		case "true":
			return true, ""
		case "false":
			return false, ""
		}
		return nil, "must be true or false"
	case KindTime:
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, "must be an RFC 3339 timestamp"
		}
		return ts.UTC(), ""
	}
	return nil, "cannot be compared"
}
