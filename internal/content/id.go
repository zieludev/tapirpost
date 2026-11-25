package content

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// lastID is the timestamp part of the newest id, so that the next one can be
// made larger.
var lastID struct {
	sync.Mutex
	stamp uint64
}

// NewID returns a UUIDv7. Its first 60 bits are a timestamp, so sorting by id
// is sorting by creation time and new rows land next to each other in the
// primary key index.
//
// Within one process the ids strictly increase, which makes "newest first" a
// stable order even for entries created in the same millisecond. The 12 bits
// after the milliseconds carry the fraction of the millisecond, as RFC 9562
// allows, and a clock that stands still or steps back is overtaken by
// counting up from the previous id.
func NewID() string {
	now := time.Now()
	stamp := uint64(now.UnixMilli())<<12 | uint64(now.Nanosecond()%1_000_000)*4096/1_000_000

	lastID.Lock()
	if stamp <= lastID.stamp {
		stamp = lastID.stamp + 1
	}
	lastID.stamp = stamp
	lastID.Unlock()

	var b [16]byte
	rand.Read(b[8:])
	ms, fraction := stamp>>12, stamp&0xfff
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = 0x70 | byte(fraction>>8)
	b[7] = byte(fraction)
	b[8] = b[8]&0x3f | 0x80

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}
