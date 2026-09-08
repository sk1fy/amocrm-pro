package crmevents

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// Cursors bind a position to one normalized query and tenant. They are not
// snapshot tokens: a later page observes later commits and retention deletes.
type cursor struct {
	Version     int    `json:"v"`
	Fingerprint string `json:"q"`
	At          int64  `json:"at"`
	ID          string `json:"id"`
}

func queryFingerprint(q serviceapi.Query, p serviceapi.Principal) string {
	order := q.Order
	if order == "" {
		order = "asc"
	}
	data, _ := json.Marshal(struct {
		Version               int              `json:"version"`
		Scope                 serviceapi.Scope `json:"scope"`
		From                  int64            `json:"from"`
		To                    int64            `json:"to"`
		UserIDs               []int64          `json:"user_ids"`
		Types                 []string         `json:"types"`
		TypePrefix            string           `json:"type_prefix"`
		EntityType            string           `json:"entity_type"`
		EntityIDs             []int64          `json:"entity_ids"`
		Order                 string           `json:"order"`
		Compact               bool             `json:"compact"`
		Categories            []string         `json:"categories,omitempty"`
		IncludeUnknownAuthors bool             `json:"include_unknown_authors,omitempty"`
		DirectoryUserIDs      []int64          `json:"directory_user_ids,omitempty"`
		Timezone              string           `json:"timezone,omitempty"`
		Buckets               string           `json:"buckets,omitempty"`
	}{
		Version: serviceapi.EventReadVersion, Scope: p.Scope,
		From: q.From, To: q.To, UserIDs: sortedUnique(q.UserIDs),
		Types: sortedUnique(q.Types), TypePrefix: q.TypePrefix,
		EntityType: q.EntityType, EntityIDs: sortedUnique(q.EntityIDs),
		Order: order, Compact: q.Compact,
		Categories: sortedUnique(q.Categories), IncludeUnknownAuthors: q.IncludeUnknownAuthors,
		DirectoryUserIDs: sortedUnique(q.DirectoryUserIDs), Timezone: q.Timezone, Buckets: q.Buckets,
	})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func sortedUnique[T ~string | ~int64](values []T) []T {
	// Normalize nil and empty slices identically without mutating caller input.
	result := append([]T{}, values...)
	slices.Sort(result)
	return slices.Compact(result)
}

func decodeCursor(q serviceapi.Query, p serviceapi.Principal) (cursor, error) {
	var after cursor
	if q.Cursor == "" {
		return after, nil
	}
	encoded, err := base64.RawURLEncoding.DecodeString(q.Cursor)
	if err != nil || json.Unmarshal(encoded, &after) != nil || after.Version != serviceapi.EventReadVersion || after.Fingerprint != queryFingerprint(q, p) || after.At < q.From || after.At > q.To || after.ID == "" {
		return cursor{}, serviceapi.Fail(serviceapi.InvalidArgument, "invalid or incompatible event cursor; restart from the first page")
	}
	return after, nil
}

func encodeCursor(q serviceapi.Query, p serviceapi.Principal, last serviceapi.Event) string {
	encoded, _ := json.Marshal(cursor{Version: serviceapi.EventReadVersion, Fingerprint: queryFingerprint(q, p), At: last.CreatedAt, ID: last.ID})
	return base64.RawURLEncoding.EncodeToString(encoded)
}
