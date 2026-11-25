package content

import (
	"encoding/json"
	"time"
)

// Status says where an entry is in the draft and publish cycle.
type Status string

// The states an entry can be in.
const (
	StatusDraft     Status = "draft"     // never published, or unpublished
	StatusPublished Status = "published" // the published copy equals the draft
	StatusChanged   Status = "changed"   // published, with newer edits in the draft
)

// Entry is one piece of content of a type.
//
// Data is the working copy that editors change. Published is the copy the
// delivery API serves: it only moves when the entry is published, so editing
// a live entry never changes what readers see.
type Entry struct {
	ID               string
	Type             string
	Data             json.RawMessage
	Published        json.RawMessage // nil when the entry is not published
	Version          int64           // goes up with every change of Data
	PublishedVersion int64           // the Version that was published, 0 if none
	CreatedAt        time.Time
	UpdatedAt        time.Time
	PublishedAt      time.Time // zero when the entry is not published
}

// Status derives the state of the entry from its versions.
func (e Entry) Status() Status {
	switch {
	case e.Published == nil:
		return StatusDraft
	case e.PublishedVersion == e.Version:
		return StatusPublished
	default:
		return StatusChanged
	}
}

// Page is one page of a listing together with the size of the whole result.
type Page struct {
	Entries []Entry
	Total   int
	Limit   int
	Offset  int
}
