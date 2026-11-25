package content

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// timeLayout has a fixed width, so that stored timestamps compare as strings
// in the same order as the moments they name. time.RFC3339Nano drops trailing
// zeros and does not have that property.
const timeLayout = "2006-01-02T15:04:05.000000Z"

var patterns sync.Map // pattern string -> *regexp.Regexp

func compilePattern(p string) (*regexp.Regexp, error) {
	if re, ok := patterns.Load(p); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, err
	}
	patterns.Store(p, re)
	return re, nil
}

// normalize checks the fields of an entry against the type and returns them
// in canonical JSON: known fields only, no nulls, timestamps in UTC, keys
// sorted. Required fields are only enforced when complete is true, because a
// draft is allowed to be unfinished.
func (t Type) normalize(raw json.RawMessage, complete bool) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var in map[string]any
	if err := dec.Decode(&in); err != nil {
		return nil, invalid("fields", "must be a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid("fields", "must be a single JSON object")
	}
	if in == nil {
		return nil, invalid("fields", "must be a JSON object")
	}

	var is issues
	for name := range in {
		if _, ok := t.Field(name); !ok {
			is.add(name, "is not a field of %q", t.Name)
		}
	}

	out := make(map[string]any, len(t.Fields))
	for _, f := range t.Fields {
		v, present := in[f.Name]
		if !present || v == nil {
			if f.Required && complete {
				is.add(f.Name, "is required")
			}
			continue
		}
		if nv, ok := f.normalize(v, &is); ok {
			out[f.Name] = nv
		}
	}
	if err := is.err(); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func (f Field) normalize(v any, is *issues) (any, bool) {
	switch f.Type {
	case FieldString, FieldText:
		s, ok := v.(string)
		if !ok {
			is.add(f.Name, "must be a string")
			return nil, false
		}
		return s, f.checkText(s, is)

	case FieldInteger:
		n, ok := v.(json.Number)
		if !ok {
			is.add(f.Name, "must be an integer")
			return nil, false
		}
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			is.add(f.Name, "must be an integer between %d and %d", int64(math.MinInt64), int64(math.MaxInt64))
			return nil, false
		}
		return i, f.checkRange(float64(i), is)

	case FieldNumber:
		n, ok := v.(json.Number)
		if !ok {
			is.add(f.Name, "must be a number")
			return nil, false
		}
		x, err := n.Float64()
		if err != nil || math.IsInf(x, 0) || math.IsNaN(x) {
			is.add(f.Name, "must be a number that fits in a 64-bit float")
			return nil, false
		}
		return x, f.checkRange(x, is)

	case FieldBoolean:
		b, ok := v.(bool)
		if !ok {
			is.add(f.Name, "must be true or false")
		}
		return b, ok

	case FieldDateTime:
		s, ok := v.(string)
		if !ok {
			is.add(f.Name, "must be an RFC 3339 timestamp")
			return nil, false
		}
		norm, err := normalizeTime(s)
		if err != nil {
			is.add(f.Name, "must be an RFC 3339 timestamp, for example 2026-01-31T12:00:00Z")
			return nil, false
		}
		return norm, true

	case FieldEnum:
		s, ok := v.(string)
		if !ok {
			is.add(f.Name, "must be a string")
			return nil, false
		}
		for _, allowed := range f.Values {
			if s == allowed {
				return s, true
			}
		}
		is.add(f.Name, "must be one of: %s", strings.Join(f.Values, ", "))
		return nil, false

	case FieldJSON:
		if hasNUL(v) {
			is.add(f.Name, "must not contain the NUL character")
			return nil, false
		}
		return v, true
	}
	is.add(f.Name, "has an unknown type %q", f.Type)
	return nil, false
}

func (f Field) checkText(s string, is *issues) bool {
	ok := true
	fail := func(format string, args ...any) {
		is.add(f.Name, format, args...)
		ok = false
	}
	// PostgreSQL cannot store NUL in text or jsonb, so it is refused for
	// every backend to keep them interchangeable.
	if strings.ContainsRune(s, 0) {
		fail("must not contain the NUL character")
	}
	if f.Type == FieldString && strings.ContainsAny(s, "\r\n") {
		fail("must be a single line; use a text field for longer content")
	}
	length := utf8.RuneCountInString(s)
	if f.MinLength != nil && length < *f.MinLength {
		fail("must be at least %d characters long", *f.MinLength)
	}
	if f.MaxLength != nil && length > *f.MaxLength {
		fail("must be at most %d characters long", *f.MaxLength)
	}
	if f.Pattern != "" {
		re, err := compilePattern(f.Pattern)
		if err != nil || !re.MatchString(s) {
			fail("must match %s", f.Pattern)
		}
	}
	return ok
}

func (f Field) checkRange(x float64, is *issues) bool {
	ok := true
	if f.Min != nil && x < *f.Min {
		is.add(f.Name, "must be at least %s", strconv.FormatFloat(*f.Min, 'g', -1, 64))
		ok = false
	}
	if f.Max != nil && x > *f.Max {
		is.add(f.Name, "must be at most %s", strconv.FormatFloat(*f.Max, 'g', -1, 64))
		ok = false
	}
	return ok
}

func normalizeTime(s string) (string, error) {
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "", err
	}
	ts = ts.UTC()
	if ts.Year() < 0 || ts.Year() > 9999 {
		return "", errors.New("year out of range")
	}
	return ts.Format(timeLayout), nil
}

func hasNUL(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.ContainsRune(x, 0)
	case []any:
		for _, e := range x {
			if hasNUL(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if strings.ContainsRune(k, 0) || hasNUL(e) {
				return true
			}
		}
	}
	return false
}
