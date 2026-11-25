// Package content is the domain of TapirPost: content types, entries, API tokens,
// the rules that validate them and the service that applies those rules.
package content

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Errors that callers are expected to test for with errors.Is.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrInvalidToken = errors.New("invalid token")
)

// Issue is one broken rule. Field is empty when the problem is not tied to a
// single field.
type Issue struct {
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// ValidationError reports input that breaks one or more rules.
type ValidationError struct {
	Issues []Issue
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Issues))
	for i, issue := range e.Issues {
		if issue.Field == "" {
			parts[i] = issue.Message
		} else {
			parts[i] = issue.Field + " " + issue.Message
		}
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// issues collects problems while validating and turns them into one error.
type issues []Issue

func (is *issues) add(field, format string, args ...any) {
	*is = append(*is, Issue{Field: field, Message: fmt.Sprintf(format, args...)})
}

func (is issues) err() error {
	if len(is) == 0 {
		return nil
	}
	sorted := make([]Issue, len(is))
	copy(sorted, is)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Field < sorted[j].Field })
	return &ValidationError{Issues: sorted}
}

func invalid(field, format string, args ...any) error {
	return &ValidationError{Issues: []Issue{{Field: field, Message: fmt.Sprintf(format, args...)}}}
}
