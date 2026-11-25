package httpapi

import (
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/zieludev/tapirpost/internal/content"
)

// filterParam matches filter[field] and filter[field][operator].
var filterParam = regexp.MustCompile(`^filter\[([^\[\]]*)\](?:\[([^\[\]]*)\])?$`)

// parseListParams reads the query string of a listing. It only knows the
// syntax; whether a field exists or an operator fits it is for the content
// type to say.
//
// Unknown parameters are an error rather than ignored. A misspelt filter
// would otherwise return everything, and every distinct junk parameter would
// take a cache entry of its own.
func parseListParams(values url.Values) (content.QueryParams, error) {
	var (
		params content.QueryParams
		issues []content.Issue
	)
	problem := func(param, message string) {
		issues = append(issues, content.Issue{Field: param, Message: message})
	}
	single := func(key string) (string, bool) {
		if len(values[key]) != 1 {
			problem(key, "must be given once")
			return "", false
		}
		return values[key][0], true
	}

	// In a fixed order, so that the same request always builds the same
	// query and reports its problems in the same order.
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	for _, key := range keys {
		switch key {
		case "limit", "offset":
			raw, ok := single(key)
			if !ok {
				continue
			}
			n, err := strconv.Atoi(raw)
			if err != nil {
				problem(key, "must be a whole number")
				continue
			}
			if key == "limit" {
				params.Limit = &n
			} else {
				params.Offset = &n
			}
		case "sort":
			raw, ok := single(key)
			if !ok {
				continue
			}
			for part := range strings.SplitSeq(raw, ",") {
				if part = strings.TrimSpace(part); part != "" {
					params.Sort = append(params.Sort, part)
				}
			}
			if len(params.Sort) == 0 {
				problem(key, "must name at least one field")
			}
		default:
			m := filterParam.FindStringSubmatch(key)
			if m == nil {
				problem(key, "is not a parameter of this route")
				continue
			}
			op := m[2]
			if op == "" {
				op = string(content.OpEq)
			}
			for _, value := range values[key] {
				params.Filters = append(params.Filters, content.RawFilter{Field: m[1], Op: op, Value: value})
			}
		}
	}
	if len(issues) > 0 {
		return content.QueryParams{}, badRequest("the query parameters are not valid", issues...)
	}
	return params, nil
}

// queryError turns a listing request that the content type refused into a
// 400: what was wrong is the query string, not a submitted entity.
func queryError(err error) error {
	var validation *content.ValidationError
	if errors.As(err, &validation) {
		return badRequest("the query parameters are not valid", validation.Issues...)
	}
	return err
}
