// Package crmreferences provides product-neutral, bounded CRM reference snapshots.
package crmreferences

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

var ErrIncomplete = errors.New("references_incomplete")
var ErrTooLarge = errors.New("references_too_large")
var ErrUnavailable = errors.New("references_unavailable")

const MaxBytes = 4 << 20

type Scope struct {
	CompanyID      string `json:"companyId"`
	AccountID      string `json:"accountId"`
	IntegrationID  string `json:"integrationId"`
	InstallationID string `json:"installationId"`
	Generation     int64  `json:"generation"`
	Product        string `json:"product"`
}
type User struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	IsActive bool    `json:"isActive"`
	GroupID  *string `json:"groupId"`
}
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Status struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Sort  int     `json:"sort"`
	Color *string `json:"color"`
}
type LeadStatus struct {
	Status
	Kind string `json:"kind"`
}
type Pipeline struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Sort     int          `json:"sort"`
	Statuses []LeadStatus `json:"statuses"`
}
type Customers struct {
	Availability string    `json:"availability"`
	Statuses     []Status  `json:"statuses"`
	CheckedAt    time.Time `json:"checkedAt"`
	ErrorCode    *string   `json:"errorCode"`
}
type Provenance struct {
	Source         string    `json:"source"`
	FetchedAt      time.Time `json:"fetchedAt"`
	FetchStartedAt time.Time `json:"fetchStartedAt"`
}
type Snapshot struct {
	SchemaVersion int        `json:"schemaVersion"`
	Scope         Scope      `json:"scope"`
	Revision      string     `json:"revision"`
	Provenance    Provenance `json:"provenance"`
	FreshUntil    time.Time  `json:"freshUntil"`
	Complete      bool       `json:"complete"`
	Users         []User     `json:"users"`
	Groups        []Group    `json:"groups"`
	Pipelines     []Pipeline `json:"pipelines"`
	Customers     Customers  `json:"customers"`
}
type Data struct {
	Users     []User
	Groups    []Group
	Pipelines []Pipeline
	Customers Customers
}
type Reader interface {
	CRMReferences(context.Context, uuid.UUID, int64) (Data, error)
}

// Fence is called on misses and hits, including after network reads and immediately
// before returning data. The product adapter owns policy and exact identity checks.
type Fence func(context.Context) error
type Provider struct {
	mu      sync.Mutex
	cache   map[string]Snapshot
	flights singleflight.Group
	Now     func() time.Time
}

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}
func (p *Provider) Invalidate(key string) { p.mu.Lock(); delete(p.cache, key); p.mu.Unlock() }
func (p *Provider) Get(ctx context.Context, key string, scope Scope, reader Reader, fence Fence) (Snapshot, error) {
	if err := fence(ctx); err != nil {
		p.Invalidate(key)
		return Snapshot{}, err
	}
	result := p.flights.DoChan(key, func() (any, error) {
		if err := fence(ctx); err != nil {
			return Snapshot{}, err
		}
		p.mu.Lock()
		cached, ok := p.cache[key]
		p.mu.Unlock()
		if ok && p.now().Before(cached.FreshUntil) && p.now().Sub(cached.Provenance.FetchStartedAt) < 120*time.Second {
			return cached, nil
		}
		started := p.now()
		fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		installation, err := uuid.Parse(scope.InstallationID)
		if err != nil {
			return Snapshot{}, ErrIncomplete
		}
		account, err := strconv.ParseInt(scope.AccountID, 10, 64)
		if err != nil {
			return Snapshot{}, ErrIncomplete
		}
		d, err := reader.CRMReferences(fetchCtx, installation, account)
		if err != nil {
			return Snapshot{}, err
		}
		out := Snapshot{SchemaVersion: 1, Scope: scope, Complete: true, Users: d.Users, Groups: d.Groups, Pipelines: d.Pipelines, Customers: d.Customers, Provenance: Provenance{"coreAmocrm", p.now(), started}, FreshUntil: started.Add(5 * time.Minute)}
		if err = Finalize(&out); err != nil {
			return Snapshot{}, err
		}
		if err = fetchCtx.Err(); err != nil {
			return Snapshot{}, err
		}
		if err = fence(fetchCtx); err != nil {
			return Snapshot{}, err
		}
		if !p.now().Before(out.FreshUntil) {
			return Snapshot{}, ErrUnavailable
		}
		p.mu.Lock()
		if p.cache == nil {
			p.cache = map[string]Snapshot{}
		}
		if len(p.cache) >= 256 {
			var oldest string
			var when time.Time
			for k, v := range p.cache {
				if oldest == "" || v.Provenance.FetchStartedAt.Before(when) {
					oldest = k
					when = v.Provenance.FetchStartedAt
				}
			}
			delete(p.cache, oldest)
		}
		p.cache[key] = out
		p.mu.Unlock()
		return out, nil
	})
	select {
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case r := <-result:
		if r.Err != nil {
			p.Invalidate(key)
			return Snapshot{}, r.Err
		}
		if err := fence(ctx); err != nil {
			p.Invalidate(key)
			return Snapshot{}, err
		}
		// Return a copy: consumer mutation must never alter future cache hits.
		b, _ := json.Marshal(r.Val)
		var out Snapshot
		_ = json.Unmarshal(b, &out)
		return out, nil
	}
}
func validID(id string) bool {
	n, e := strconv.ParseInt(id, 10, 64)
	return e == nil && n > 0 && n <= 9007199254740991 && strconv.FormatInt(n, 10) == id
}
func numeric(a, b string) bool {
	x, _ := strconv.ParseInt(a, 10, 64)
	y, _ := strconv.ParseInt(b, 10, 64)
	return x < y
}
func safeSort(n int) bool { return int64(n) >= -9007199254740991 && int64(n) <= 9007199254740991 }
func Finalize(s *Snapshot) error {
	if s.Users == nil || s.Groups == nil || s.Pipelines == nil || s.Customers.Statuses == nil {
		return ErrIncomplete
	}
	if len(s.Users) > 1000 || len(s.Groups) > 1000 || len(s.Pipelines) > 1000 || len(s.Customers.Statuses) > 1000 {
		return ErrTooLarge
	}
	groups := map[string]bool{}
	users := map[string]bool{}
	pipelines := map[string]bool{}
	stagePipeline := map[string]string{}
	statuses := map[string]bool{}
	for _, g := range s.Groups {
		if !validID(g.ID) || groups[g.ID] {
			return ErrIncomplete
		}
		groups[g.ID] = true
	}
	for _, u := range s.Users {
		if !validID(u.ID) || users[u.ID] || u.GroupID != nil && !groups[*u.GroupID] {
			return ErrIncomplete
		}
		users[u.ID] = true
	}
	for i := range s.Pipelines {
		p := &s.Pipelines[i]
		if !validID(p.ID) || pipelines[p.ID] || p.Statuses == nil || !safeSort(p.Sort) {
			return ErrIncomplete
		}
		pipelines[p.ID] = true
		if len(p.Statuses) > 1000 {
			return ErrTooLarge
		}
		seen := map[string]bool{}
		for _, st := range p.Statuses {
			if !validID(st.ID) || !safeSort(st.Sort) || seen[st.ID] || (st.Kind != "open" && st.Kind != "won" && st.Kind != "lost") {
				return ErrIncomplete
			}
			seen[st.ID] = true
			expectedKind := "open"
			if st.ID == "142" {
				expectedKind = "won"
			}
			if st.ID == "143" {
				expectedKind = "lost"
			}
			if st.Kind != expectedKind {
				return ErrIncomplete
			}
			if expectedKind == "open" {
				if previous, ok := stagePipeline[st.ID]; ok && previous != p.ID {
					return ErrIncomplete
				}
				stagePipeline[st.ID] = p.ID
			}
		}
		sort.Slice(p.Statuses, func(i, j int) bool { return numeric(p.Statuses[i].ID, p.Statuses[j].ID) })
	}
	for _, st := range s.Customers.Statuses {
		if !validID(st.ID) || !safeSort(st.Sort) || statuses[st.ID] {
			return ErrIncomplete
		}
		statuses[st.ID] = true
	}
	if s.Customers.Availability != "supported" && s.Customers.Availability != "unsupported" && s.Customers.Availability != "unknown" {
		return ErrIncomplete
	}
	if s.Customers.Availability != "supported" && len(s.Customers.Statuses) != 0 {
		return ErrIncomplete
	}
	sort.Slice(s.Users, func(i, j int) bool { return numeric(s.Users[i].ID, s.Users[j].ID) })
	sort.Slice(s.Groups, func(i, j int) bool { return numeric(s.Groups[i].ID, s.Groups[j].ID) })
	sort.Slice(s.Pipelines, func(i, j int) bool { return numeric(s.Pipelines[i].ID, s.Pipelines[j].ID) })
	sort.Slice(s.Customers.Statuses, func(i, j int) bool { return numeric(s.Customers.Statuses[i].ID, s.Customers.Statuses[j].ID) })
	// All keys in this DTO are ASCII and all numbers are bounded integers. A map
	// encoding sorts keys exactly as JCS; disable HTML escaping and restore U+2028/29.
	semantic := map[string]any{"scope": s.Scope, "users": s.Users, "groups": s.Groups, "pipelines": s.Pipelines, "customers": map[string]any{"availability": s.Customers.Availability, "statuses": s.Customers.Statuses}}
	raw, err := json.Marshal(semantic)
	if err != nil {
		return ErrIncomplete
	}
	var mapped any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&mapped) != nil {
		return ErrIncomplete
	}
	canonical, err := canonicalJSON(mapped)
	if err != nil {
		return ErrIncomplete
	}
	sum := sha256.Sum256(canonical)
	s.Revision = "sha256:" + hex.EncodeToString(sum[:])
	out, err := json.Marshal(s)
	if err != nil {
		return ErrIncomplete
	}
	if len(out) > MaxBytes {
		return ErrTooLarge
	}
	return nil
}
func canonicalJSON(value any) ([]byte, error) {
	// json.Marshal uses escaping compatible with ECMAScript except HTML and the two
	// line separators. Decode/encode every string explicitly to preserve literal
	// backslash-u sequences while allowing literal Unicode separator characters.
	switch v := value.(type) {
	case string:
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
		r := strings.TrimSuffix(b.String(), "\n")
		// Replace escapes only when preceded by an even number of backslashes.
		var out strings.Builder
		for i := 0; i < len(r); {
			if r[i] == '\\' && i+1 < len(r) {
				if strings.HasPrefix(r[i:], `\u2028`) {
					out.WriteRune('\u2028')
					i += 6
					continue
				}
				if strings.HasPrefix(r[i:], `\u2029`) {
					out.WriteRune('\u2029')
					i += 6
					continue
				}
				out.WriteString(r[i : i+2])
				i += 2
				continue
			}
			out.WriteByte(r[i])
			i++
		}
		return []byte(out.String()), nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b bytes.Buffer
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := canonicalJSON(k)
			b.Write(kb)
			b.WriteByte(':')
			vb, e := canonicalJSON(v[k])
			if e != nil {
				return nil, e
			}
			b.Write(vb)
		}
		b.WriteByte('}')
		return b.Bytes(), nil
	case []any:
		var b bytes.Buffer
		b.WriteByte('[')
		for i, x := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			xb, e := canonicalJSON(x)
			if e != nil {
				return nil, e
			}
			b.Write(xb)
		}
		b.WriteByte(']')
		return b.Bytes(), nil
	default:
		return json.Marshal(value)
	}
}
