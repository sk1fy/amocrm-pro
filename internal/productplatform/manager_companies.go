package productplatform

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type companyRuntimeAccount struct {
	AccountID    uuid.UUID `json:"account_id"`
	CRMAccountID string    `json:"customer_account_id"`
}
type companySummary struct {
	ID                companyProjectionID     `json:"id"`
	CRMID             string                  `json:"crm_company_id"`
	Name              string                  `json:"name"`
	CustomerAccounts  []string                `json:"customer_account_ids"`
	SourceResolution  string                  `json:"source_resolution"`
	RuntimeResolution string                  `json:"runtime_resolution"`
	RuntimeAccounts   []companyRuntimeAccount `json:"runtime_accounts"`
}
type companyPage struct {
	Items  []companySummary `json:"items"`
	Next   *string          `json:"next_cursor"`
	Source companySource    `json:"source"`
}
type companyContactLink struct {
	CRMID     string     `json:"crm_company_id"`
	CompanyID *uuid.UUID `json:"company_id"`
}
type companyContact struct {
	ID        string               `json:"id"`
	Name      string               `json:"name"`
	Companies []companyContactLink `json:"companies"`
}
type companyDetail struct {
	companySummary
	Source              companySource        `json:"source"`
	Contacts            []companyContact     `json:"contacts"`
	UnresolvedRelations []companyContactLink `json:"unresolved_relations"`
}
type companyFilters struct {
	Q     string
	Limit int
}
type companySeek struct {
	companyActivation
	Generation uuid.UUID
	After      int64
	Expires    time.Time
}

func (f companyFilters) hash() [32]byte { b, _ := json.Marshal(f); return sha256.Sum256(b) }
func (o ManagerOptions) companyCursor(s companySeek, a managerActor, f companyFilters) string {
	b, _ := json.Marshal(s)
	return base64.RawURLEncoding.EncodeToString(append(b, o.routeCursorMAC("platform-companies-v1", b, a, f.hash())...))
}
func (o ManagerOptions) parseCompanyCursor(raw string, a managerActor, f companyFilters) (companySeek, error) {
	var s companySeek
	b, err := managerBase64(raw)
	if err != nil || len(b) <= 32 || len(b) > 512 {
		return s, errManagerCursor
	}
	data := b[:len(b)-32]
	if !hmac.Equal(b[len(b)-32:], o.routeCursorMAC("platform-companies-v1", data, a, f.hash())) || json.Unmarshal(data, &s) != nil || s.Scope == uuid.Nil || s.Generation == uuid.Nil || s.Epoch <= 0 || s.After <= 0 || !s.Expires.After(time.Now()) {
		return s, errManagerCursor
	}
	return s, nil
}
func companySummaries(ctx context.Context, tx pgx.Tx, g uuid.UUID, f companyFilters, after int64, id *uuid.UUID) ([]companySummary, error) {
	rows, err := tx.Query(ctx, `SELECT company_id,crm_company_id::text,name,source_resolution FROM platform_company.company_rows WHERE generation_id=$1 AND crm_company_id>$2 AND ($3::uuid IS NULL OR company_id=$3) AND ($4='' OR strpos(lower(name),lower($4))>0) ORDER BY crm_company_id LIMIT $5`, g, after, id, f.Q, f.Limit+1)
	if err != nil {
		return nil, err
	}
	result, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (companySummary, error) {
		var c companySummary
		var id uuid.UUID
		err := r.Scan(&id, &c.CRMID, &c.Name, &c.SourceResolution)
		c.ID = companyProjectionID(id)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	for i := range result {
		c := &result[i]
		c.CustomerAccounts = []string{}
		c.RuntimeAccounts = []companyRuntimeAccount{}
		c.RuntimeResolution = "unresolved"
		rows, err := tx.Query(ctx, `SELECT a.account_id::text,(SELECT count(*) FROM platform_company.company_account_claims b WHERE b.generation_id=a.generation_id AND b.account_id=a.account_id),v.account_id
 FROM platform_company.company_account_claims a LEFT JOIN platform_manager.visible_accounts v ON v.amo_account_id=a.account_id
 WHERE a.generation_id=$1 AND a.company_id=$2 ORDER BY a.account_id`, g, uuid.UUID(c.ID))
		if err != nil {
			return nil, err
		}
		type claim struct {
			account string
			count   int
			runtime *uuid.UUID
		}
		claims, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (claim, error) {
			var c claim
			err := r.Scan(&c.account, &c.count, &c.runtime)
			return c, err
		})
		if err != nil {
			return nil, err
		}
		ambiguous := false
		for _, a := range claims {
			c.CustomerAccounts = append(c.CustomerAccounts, a.account)
			if a.count > 1 {
				ambiguous = true
				continue
			}
			if a.runtime != nil {
				c.RuntimeAccounts = append(c.RuntimeAccounts, companyRuntimeAccount{*a.runtime, a.account})
			}
		}
		if ambiguous {
			c.RuntimeResolution = "ambiguous"
		} else if len(c.RuntimeAccounts) == len(c.CustomerAccounts) && len(c.RuntimeAccounts) > 0 {
			c.RuntimeResolution = "resolved"
		} else if len(c.RuntimeAccounts) > 0 {
			c.RuntimeResolution = "partial"
		}
	}
	return result, nil
}
func (m managerHTTP) listCompanies(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	f := companyFilters{Limit: 25}
	q, err := boundedAccountQuery(r.URL.RawQuery)
	var raw string
	if err == nil {
		for key, values := range q {
			if len(values) != 1 {
				err = errManagerInput
				break
			}
			switch key {
			case "q":
				f.Q = values[0]
				if !managerText(f.Q, 0, 200) {
					err = errManagerInput
				}
			case "limit":
				f.Limit, err = accountInteger(values[0], 1, 100)
			case "cursor":
				raw = values[0]
			default:
				err = errManagerInput
			}
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		writeManagerError(w, 400, "validation_failed", env.RequestID, 0)
		return
	}
	var seek companySeek
	if raw != "" {
		seek, err = m.options.parseCompanyCursor(raw, env.Actor, f)
		if err != nil {
			writeManagerStoreError(w, env.RequestID, err)
			return
		}
	}
	page, next, err := m.store.companies(r.Context(), f, seek)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	if next.Generation != uuid.Nil {
		cursor := m.options.companyCursor(next, env.Actor, f)
		page.Next = &cursor
	}
	writeLifecycleJSON(w, 200, page)
}
func (s managerStore) companies(ctx context.Context, f companyFilters, seek companySeek) (companyPage, companySeek, error) {
	var page companyPage
	var next companySeek
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return page, next, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pinned *uuid.UUID
	if seek.Generation != uuid.Nil {
		pinned = &seek.Generation
	}
	p, err := readCompanyProjection(ctx, tx, companyClients, pinned)
	if err != nil {
		return page, next, err
	}
	if pinned != nil && (p.activation != seek.companyActivation || !seek.Expires.After(time.Now())) {
		return page, next, errManagerCursor
	}
	page.Source = p.source
	if p.generation != nil {
		page.Items, err = companySummaries(ctx, tx, *p.generation, f, seek.After, nil)
		if err != nil {
			return page, next, err
		}
		if len(page.Items) > f.Limit {
			page.Items = page.Items[:f.Limit]
			after, _ := strconv.ParseInt(page.Items[len(page.Items)-1].CRMID, 10, 64)
			expiry := seek.Expires
			if expiry.IsZero() {
				expiry = time.Now().Add(15 * time.Minute)
			}
			next = companySeek{p.activation, *p.generation, after, expiry}
		}
	}
	return page, next, tx.Commit(ctx)
}
func (m managerHTTP) getCompany(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, err := managerUUID(r.PathValue("id"))
	if err != nil || r.URL.RawQuery != "" {
		writeManagerError(w, 400, "validation_failed", env.RequestID, 0)
		return
	}
	result, err := m.store.company(r.Context(), id)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	writeLifecycleJSON(w, 200, result)
}
func (s managerStore) company(ctx context.Context, id uuid.UUID) (companyDetail, error) {
	var result companyDetail
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p, err := readCompanyProjection(ctx, tx, companyClients, nil)
	if err != nil {
		return result, err
	}
	if p.generation == nil {
		return result, errManagerNotFound
	}
	result.Source = p.source
	companies, err := companySummaries(ctx, tx, *p.generation, companyFilters{Limit: 1}, 0, &id)
	if err != nil {
		return result, err
	}
	if len(companies) != 1 {
		return result, errManagerNotFound
	}
	result.companySummary = companies[0]
	rows, err := tx.Query(ctx, `SELECT c.crm_contact_id::text,c.name,(SELECT jsonb_agg(jsonb_build_object('crm_company_id',l.crm_company_id::text,'company_id',l.company_id) ORDER BY l.crm_company_id) FROM platform_company.contact_company_links l WHERE l.generation_id=c.generation_id AND l.crm_contact_id=c.crm_contact_id)
 FROM platform_company.contact_rows c WHERE c.generation_id=$1 AND EXISTS(SELECT FROM platform_company.contact_company_links l WHERE l.generation_id=c.generation_id AND l.crm_contact_id=c.crm_contact_id AND l.company_id=$2) ORDER BY c.crm_contact_id`, *p.generation, id)
	if err != nil {
		return result, err
	}
	result.Contacts, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (companyContact, error) {
		var c companyContact
		err := r.Scan(&c.ID, &c.Name, &c.Companies)
		return c, err
	})
	if err != nil {
		return result, err
	}
	result.UnresolvedRelations = []companyContactLink{}
	for _, c := range result.Contacts {
		for _, l := range c.Companies {
			if l.CompanyID == nil {
				result.UnresolvedRelations = append(result.UnresolvedRelations, l)
			}
		}
	}
	return result, tx.Commit(ctx)
}
