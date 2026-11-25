package content

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"
)

// Scope is a permission carried by a token.
type Scope string

// The scopes, each one including the ones before it.
const (
	ScopeRead  Scope = "read"  // published content through the delivery API
	ScopeWrite Scope = "write" // entries: drafts, publishing, deleting
	ScopeAdmin Scope = "admin" // content types and tokens
)

const (
	tokenPrefix    = "tapirpost_"
	tokenRandBytes = 32
	// MinRootTokenLen is the shortest root token the service accepts.
	MinRootTokenLen = 32
	maxTokenNameLen = 100
	// RootTokenID identifies the token that comes from configuration.
	RootTokenID = "root"
)

// Token is an API credential. The secret itself is never stored: only its
// SHA-256 hash is, and Prefix keeps enough of it to recognise a token in a
// listing.
type Token struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	Scopes    []Scope    `json:"scopes"`
	Types     []string   `json:"types"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// TokenSpec is what a client supplies to create a token. An empty Types list
// means every content type.
type TokenSpec struct {
	Name      string     `json:"name"`
	Scopes    []Scope    `json:"scopes"`
	Types     []string   `json:"types"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// Allows reports whether the token may act with the given scope on the given
// content type. An empty typeName asks about the scope alone.
func (t Token) Allows(scope Scope, typeName string) bool {
	if t.has(ScopeAdmin) {
		return true
	}
	switch scope {
	case ScopeRead:
		if !t.has(ScopeRead) && !t.has(ScopeWrite) {
			return false
		}
	case ScopeWrite:
		if !t.has(ScopeWrite) {
			return false
		}
	default:
		return false
	}
	if typeName == "" || len(t.Types) == 0 {
		return true
	}
	for _, name := range t.Types {
		if name == typeName {
			return true
		}
	}
	return false
}

func (t Token) has(scope Scope) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

func (s TokenSpec) validate(now time.Time) error {
	var is issues
	if n := utf8.RuneCountInString(strings.TrimSpace(s.Name)); n == 0 || n > maxTokenNameLen {
		is.add("name", "must be 1 to %d characters long", maxTokenNameLen)
	}
	if len(s.Scopes) == 0 {
		is.add("scopes", "must list at least one of: read, write, admin")
	}
	admin := false
	seen := make(map[Scope]bool, len(s.Scopes))
	for _, scope := range s.Scopes {
		switch scope {
		case ScopeRead, ScopeWrite:
		case ScopeAdmin:
			admin = true
		default:
			is.add("scopes", "%q is not a scope", scope)
		}
		if seen[scope] {
			is.add("scopes", "%q is listed twice", scope)
		}
		seen[scope] = true
	}
	if admin && len(s.Types) > 0 {
		is.add("types", "an admin token cannot be limited to content types")
	}
	for _, name := range s.Types {
		if !ValidName(name) {
			is.add("types", "%q is not a content type name", name)
		}
	}
	if s.ExpiresAt != nil && !s.ExpiresAt.After(now) {
		is.add("expires_at", "must be in the future")
	}
	return is.err()
}

// newSecret returns a fresh token secret.
func newSecret() string {
	b := make([]byte, tokenRandBytes)
	rand.Read(b)
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// hashSecret is a plain SHA-256: the secret is 256 random bits, so there is
// nothing for a slow password hash to protect and lookups stay cheap.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func sameSecret(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
