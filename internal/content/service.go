package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Store is the persistence the service needs. Implementations report a
// missing row as ErrNotFound and a lost race as ErrConflict.
//
// The version arguments are optimistic locks. A write names the schema
// version its data was validated against, and a publish also names the entry
// version it validated: if either moved in the meantime the store refuses
// with ErrConflict instead of storing something nobody checked.
//
// TypeRevision names the state of the published content of a type. Every
// change that can alter what Entries returns for published content must
// change it in the same transaction, and a value must never come back: not
// after any number of changes, and not when a type is deleted and created
// again under the same name. Caches are keyed by it.
type Store interface {
	CreateType(ctx context.Context, t Type) error
	Type(ctx context.Context, name string) (Type, error)
	Types(ctx context.Context) ([]Type, error)
	UpdateType(ctx context.Context, t Type, removedFields []string) (Type, error)
	DeleteType(ctx context.Context, name string) error
	TypeRevision(ctx context.Context, name string) (string, error)

	CreateEntry(ctx context.Context, e Entry, schemaVersion int64) error
	Entry(ctx context.Context, typeName, id string) (Entry, error)
	Entries(ctx context.Context, typeName string, q Query, published bool) ([]Entry, int, error)
	UpdateEntry(ctx context.Context, typeName, id string, data json.RawMessage, schemaVersion int64, at time.Time) (Entry, error)
	PublishEntry(ctx context.Context, typeName, id string, version, schemaVersion int64, at time.Time) (Entry, error)
	UnpublishEntry(ctx context.Context, typeName, id string) (Entry, error)
	DeleteEntry(ctx context.Context, typeName, id string) error

	CreateToken(ctx context.Context, t Token, hash string) error
	TokenByHash(ctx context.Context, hash string) (Token, error)
	Tokens(ctx context.Context) ([]Token, error)
	DeleteToken(ctx context.Context, id string) error

	Ping(ctx context.Context) error
}

// Service applies the content rules on top of a Store.
type Service struct {
	store     Store
	rootToken string
	now       func() time.Time
}

// NewService returns a service backed by store. A non-empty rootToken is
// accepted as an admin token without being stored anywhere.
func NewService(store Store, rootToken string) (*Service, error) {
	if rootToken != "" && len(rootToken) < MinRootTokenLen {
		return nil, fmt.Errorf("the admin token must be at least %d characters long", MinRootTokenLen)
	}
	return &Service{store: store, rootToken: rootToken, now: time.Now}, nil
}

// timestamp is the current time at the precision every store can keep.
func (s *Service) timestamp() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

// Ping reports whether the store can be reached.
func (s *Service) Ping(ctx context.Context) error {
	return s.store.Ping(ctx)
}

// CreateType stores a new content type.
func (s *Service) CreateType(ctx context.Context, spec TypeSpec) (Type, error) {
	if err := spec.validate(); err != nil {
		return Type{}, err
	}
	now := s.timestamp()
	t := Type{
		ID:            NewID(),
		Name:          spec.Name,
		Description:   spec.Description,
		Fields:        spec.Fields,
		SchemaVersion: 1,
		Revision:      1,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.store.CreateType(ctx, t); err != nil {
		return Type{}, fmt.Errorf("content type %q: %w", spec.Name, err)
	}
	return t, nil
}

// Type returns the content type with the given name.
func (s *Service) Type(ctx context.Context, name string) (Type, error) {
	if !ValidName(name) {
		return Type{}, fmt.Errorf("content type %q: %w", name, ErrNotFound)
	}
	t, err := s.store.Type(ctx, name)
	if err != nil {
		return Type{}, fmt.Errorf("content type %q: %w", name, err)
	}
	return t, nil
}

// Types returns all content types, ordered by name.
func (s *Service) Types(ctx context.Context) ([]Type, error) {
	return s.store.Types(ctx)
}

// TypeRevision returns a value that names the current state of the published
// content of the type. It changes whenever that content may have changed and
// is never used twice.
func (s *Service) TypeRevision(ctx context.Context, name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("content type %q: %w", name, ErrNotFound)
	}
	rev, err := s.store.TypeRevision(ctx, name)
	if err != nil {
		return "", fmt.Errorf("content type %q: %w", name, err)
	}
	return rev, nil
}

// UpdateType replaces the description and fields of a content type. Values
// of removed fields are deleted from every entry of the type.
func (s *Service) UpdateType(ctx context.Context, name string, spec TypeSpec) (Type, error) {
	if spec.Name == "" {
		spec.Name = name
	}
	if spec.Name != name {
		return Type{}, invalid("name", "cannot be changed")
	}
	if err := spec.validate(); err != nil {
		return Type{}, err
	}
	current, err := s.Type(ctx, name)
	if err != nil {
		return Type{}, err
	}
	removed, err := removedOrRetyped(current.Fields, spec.Fields)
	if err != nil {
		return Type{}, err
	}
	current.Description = spec.Description
	current.Fields = spec.Fields
	current.UpdatedAt = s.timestamp()
	updated, err := s.store.UpdateType(ctx, current, removed)
	if err != nil {
		return Type{}, fmt.Errorf("content type %q: %w", name, err)
	}
	return updated, nil
}

// DeleteType removes a content type together with all of its entries.
func (s *Service) DeleteType(ctx context.Context, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("content type %q: %w", name, ErrNotFound)
	}
	if err := s.store.DeleteType(ctx, name); err != nil {
		return fmt.Errorf("content type %q: %w", name, err)
	}
	return nil
}

// CreateEntry stores a new draft.
func (s *Service) CreateEntry(ctx context.Context, typeName string, fields json.RawMessage) (Entry, error) {
	t, err := s.Type(ctx, typeName)
	if err != nil {
		return Entry{}, err
	}
	data, err := t.normalize(fields, false)
	if err != nil {
		return Entry{}, err
	}
	now := s.timestamp()
	e := Entry{
		ID:        NewID(),
		Type:      t.Name,
		Data:      data,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.CreateEntry(ctx, e, t.SchemaVersion); err != nil {
		return Entry{}, fmt.Errorf("create entry of %q: %w", typeName, err)
	}
	return e, nil
}

// Entry returns an entry with its draft and, if any, its published copy.
func (s *Service) Entry(ctx context.Context, typeName, id string) (Entry, error) {
	if !ValidName(typeName) {
		return Entry{}, fmt.Errorf("content type %q: %w", typeName, ErrNotFound)
	}
	e, err := s.store.Entry(ctx, typeName, id)
	if err != nil {
		return Entry{}, fmt.Errorf("entry %q of %q: %w", id, typeName, err)
	}
	return e, nil
}

// PublishedEntry returns an entry only if it is published.
func (s *Service) PublishedEntry(ctx context.Context, typeName, id string) (Entry, error) {
	e, err := s.Entry(ctx, typeName, id)
	if err != nil {
		return Entry{}, err
	}
	if e.Published == nil {
		return Entry{}, fmt.Errorf("entry %q of %q: %w", id, typeName, ErrNotFound)
	}
	return e, nil
}

// UpdateEntry replaces the draft of an entry. What is published stays as it
// is until the entry is published again.
func (s *Service) UpdateEntry(ctx context.Context, typeName, id string, fields json.RawMessage) (Entry, error) {
	t, err := s.Type(ctx, typeName)
	if err != nil {
		return Entry{}, err
	}
	data, err := t.normalize(fields, false)
	if err != nil {
		return Entry{}, err
	}
	e, err := s.store.UpdateEntry(ctx, t.Name, id, data, t.SchemaVersion, s.timestamp())
	if err != nil {
		return Entry{}, fmt.Errorf("entry %q of %q: %w", id, typeName, err)
	}
	return e, nil
}

// PublishEntry makes the current draft the published copy. Unlike a draft,
// the published copy must satisfy every rule of the type.
func (s *Service) PublishEntry(ctx context.Context, typeName, id string) (Entry, error) {
	t, err := s.Type(ctx, typeName)
	if err != nil {
		return Entry{}, err
	}
	e, err := s.Entry(ctx, t.Name, id)
	if err != nil {
		return Entry{}, err
	}
	if _, err := t.normalize(e.Data, true); err != nil {
		return Entry{}, err
	}
	published, err := s.store.PublishEntry(ctx, t.Name, id, e.Version, t.SchemaVersion, s.timestamp())
	if err != nil {
		return Entry{}, fmt.Errorf("entry %q of %q: %w", id, typeName, err)
	}
	return published, nil
}

// UnpublishEntry withdraws the published copy and keeps the draft.
func (s *Service) UnpublishEntry(ctx context.Context, typeName, id string) (Entry, error) {
	if !ValidName(typeName) {
		return Entry{}, fmt.Errorf("content type %q: %w", typeName, ErrNotFound)
	}
	e, err := s.store.UnpublishEntry(ctx, typeName, id)
	if err != nil {
		return Entry{}, fmt.Errorf("entry %q of %q: %w", id, typeName, err)
	}
	return e, nil
}

// DeleteEntry removes an entry, published or not.
func (s *Service) DeleteEntry(ctx context.Context, typeName, id string) error {
	if !ValidName(typeName) {
		return fmt.Errorf("content type %q: %w", typeName, ErrNotFound)
	}
	if err := s.store.DeleteEntry(ctx, typeName, id); err != nil {
		return fmt.Errorf("entry %q of %q: %w", id, typeName, err)
	}
	return nil
}

// ListEntries pages through the drafts of a type.
func (s *Service) ListEntries(ctx context.Context, t Type, p QueryParams) (Page, error) {
	return s.list(ctx, t, p, false)
}

// ListPublished pages through the published copies of a type.
func (s *Service) ListPublished(ctx context.Context, t Type, p QueryParams) (Page, error) {
	return s.list(ctx, t, p, true)
}

func (s *Service) list(ctx context.Context, t Type, p QueryParams, published bool) (Page, error) {
	q, err := t.Query(p, published)
	if err != nil {
		return Page{}, err
	}
	entries, total, err := s.store.Entries(ctx, t.Name, q, published)
	if err != nil {
		return Page{}, fmt.Errorf("list entries of %q: %w", t.Name, err)
	}
	return Page{Entries: entries, Total: total, Limit: q.Limit, Offset: q.Offset}, nil
}

// CreateToken stores a new token and returns it with its secret. The secret
// cannot be recovered later.
func (s *Service) CreateToken(ctx context.Context, spec TokenSpec) (Token, string, error) {
	now := s.timestamp()
	if err := spec.validate(now); err != nil {
		return Token{}, "", err
	}
	secret := newSecret()
	t := Token{
		ID:        NewID(),
		Name:      strings.TrimSpace(spec.Name),
		Prefix:    secret[:len(tokenPrefix)+6],
		Scopes:    spec.Scopes,
		Types:     spec.Types,
		CreatedAt: now,
	}
	if t.Types == nil {
		t.Types = []string{}
	}
	if spec.ExpiresAt != nil {
		expires := spec.ExpiresAt.UTC().Truncate(time.Microsecond)
		t.ExpiresAt = &expires
	}
	if err := s.store.CreateToken(ctx, t, hashSecret(secret)); err != nil {
		return Token{}, "", fmt.Errorf("create token: %w", err)
	}
	return t, secret, nil
}

// Tokens returns all stored tokens, oldest first.
func (s *Service) Tokens(ctx context.Context) ([]Token, error) {
	return s.store.Tokens(ctx)
}

// DeleteToken revokes a token.
func (s *Service) DeleteToken(ctx context.Context, id string) error {
	if err := s.store.DeleteToken(ctx, id); err != nil {
		return fmt.Errorf("token %q: %w", id, err)
	}
	return nil
}

// Authenticate returns the token a secret belongs to, or ErrInvalidToken.
func (s *Service) Authenticate(ctx context.Context, secret string) (Token, error) {
	if s.rootToken != "" && sameSecret(secret, s.rootToken) {
		return Token{ID: RootTokenID, Name: RootTokenID, Scopes: []Scope{ScopeAdmin}, Types: []string{}}, nil
	}
	if !strings.HasPrefix(secret, tokenPrefix) {
		return Token{}, ErrInvalidToken
	}
	t, err := s.store.TokenByHash(ctx, hashSecret(secret))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Token{}, ErrInvalidToken
		}
		return Token{}, fmt.Errorf("look up token: %w", err)
	}
	if t.ExpiresAt != nil && !t.ExpiresAt.After(s.now()) {
		return Token{}, ErrInvalidToken
	}
	return t, nil
}
