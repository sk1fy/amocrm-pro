package productplatform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/companycrm"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/companysource"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
)

type companyComponent string

const (
	companyClients  companyComponent = "clients"
	companyMetadata companyComponent = "metadata"
)

func (p companyComponent) valid() bool { return p == companyClients || p == companyMetadata }

type companyActivation struct {
	Scope uuid.UUID `json:"scope_id"`
	Epoch int64     `json:"epoch"`
}

func companyRefreshKind(a companyActivation) queue.Kind {
	return queue.Kind("company.refresh." + a.Scope.String() + "." + strconv.FormatInt(a.Epoch, 10))
}

type companyJobPayload struct {
	companyActivation
	Component companyComponent `json:"component"`
}
type CompanyObserver struct {
	pool       *pgxpool.Pool
	client     *companysource.Client
	activation companyActivation
}

var ErrCompanySourceMismatch = errors.New("company source configuration does not match active binding")

func NewCompanyObserver(ctx context.Context, pool *pgxpool.Pool, client *companysource.Client) (*CompanyObserver, error) {
	o := &CompanyObserver{pool: pool, client: client}
	account, binding := client.Scope()
	err := pool.QueryRow(ctx, `SELECT scope_id,epoch FROM platform_company.register_source($1,$2)`, account, binding).Scan(&o.activation.Scope, &o.activation.Epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCompanySourceMismatch
	}
	if err != nil {
		return nil, err
	}
	return o, nil
}
func (o *CompanyObserver) Kind() queue.Kind { return companyRefreshKind(o.activation) }
func (o *CompanyObserver) Enroll(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		q, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := o.enrollCompany(q)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("Company projection enrollment unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (o *CompanyObserver) enrollCompany(ctx context.Context) error {
	tx, err := o.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var active companyActivation
	var retry *time.Time
	err = tx.QueryRow(ctx, `SELECT scope_id,epoch,retry_not_before FROM platform_company.source_control WHERE singleton FOR UPDATE`).Scan(&active.Scope, &active.Epoch, &retry)
	if err != nil {
		return err
	}
	if active != o.activation || retry != nil && retry.After(time.Now()) {
		return nil
	}
	for _, part := range []companyComponent{companyClients, companyMetadata} {
		var ready bool
		err = tx.QueryRow(ctx, `SELECT (c.next_refresh IS NULL OR c.next_refresh<=statement_timestamp()) AND (j.id IS NULL OR j.status IN ('dead','succeeded'))
 FROM platform_company.source_components c LEFT JOIN platform_jobs.jobs j ON j.id=c.current_job_id WHERE c.scope_id=$1 AND c.component=$2 FOR UPDATE OF c`, active.Scope, part).Scan(&ready)
		if err != nil {
			return err
		}
		if !ready {
			continue
		}
		id := uuid.New()

		if _, err = tx.Exec(ctx, `SELECT platform_company.enqueue_refresh($1,$2,$3,$4,$5)`, active.Scope, active.Epoch, part, id, o.Kind()); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE platform_company.source_components SET current_job_id=$3 WHERE scope_id=$1 AND component=$2`, active.Scope, part, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type companyObservation struct {
	source   companycrm.Source
	clients  *companycrm.Clients
	metadata *companycrm.WorkflowMetadata
	retry    *time.Time
	code     string
}

func (o *CompanyObserver) RefreshJob(ctx context.Context, job queue.Job) queue.Completion {
	var p companyJobPayload
	if json.Unmarshal(job.Payload, &p) != nil || p.companyActivation != o.activation || !p.Component.valid() || job.Kind != o.Kind() {
		return queue.Completion{Outcome: queue.Outcome{FailureCode: "invalid_company_job"}}
	}
	var active companyActivation
	var retry *time.Time
	var linked bool
	err := o.pool.QueryRow(ctx, `SELECT c.scope_id,c.epoch,c.retry_not_before,EXISTS(SELECT FROM platform_company.source_components s WHERE s.scope_id=c.scope_id AND s.component=$1 AND s.current_job_id=$2) FROM platform_company.source_control c`, p.Component, job.ID).Scan(&active.Scope, &active.Epoch, &retry, &linked)
	if err != nil {
		return queue.Completion{Outcome: queue.Outcome{FailureCode: "source_unavailable", Retry: true}}
	}
	if active != o.activation || !linked {
		return queue.Completion{Outcome: queue.Outcome{FailureCode: "source_scope_changed"}}
	}
	if retry != nil && retry.After(time.Now()) {
		return queue.Completion{Outcome: queue.Outcome{FailureCode: "source_rate_limited", Retry: true, RetryAfter: time.Until(*retry)}}
	}
	var obs companyObservation
	status := 0
	if p.Component == companyClients {
		r, e := o.client.Clients(ctx)
		err = e
		obs.source, obs.clients, obs.retry, status = r.Response.Source, r.Response.Data, r.RetryAt, r.Status
	} else {
		r, e := o.client.WorkflowMetadata(ctx)
		err = e
		obs.source, obs.metadata, obs.retry, status = r.Response.Source, r.Response.Data, r.RetryAt, r.Status
	}
	if err != nil {
		obs.code = "source_unavailable"
	} else if status != http.StatusOK || !obs.source.Complete {
		obs.code = "source_incomplete"
	}
	if status == 429 {
		obs.code = "source_rate_limited"
	}
	if obs.code == "" && !validCompanyObservation(obs, p.Component) {
		obs.code = "source_invalid"
	}
	return queue.Completion{Publish: func(ctx context.Context, tx pgx.Tx) (queue.Outcome, error) {
		return publishCompanyObservation(ctx, tx, p, job.ID, obs)
	}}
}
func validCompanyObservation(o companyObservation, part companyComponent) bool {
	if o.source.State != "fresh" && o.source.State != "stale" {
		return false
	}
	if part == companyClients {
		if o.clients == nil || len(o.clients.Companies) > 100000 || len(o.clients.Contacts) > 100000 {
			return false
		}
		seen := map[int64]bool{}
		for _, c := range o.clients.Companies {
			if c.CompanyID <= 0 || seen[c.CompanyID] || !managerText(c.Name, 0, 512) {
				return false
			}
			seen[c.CompanyID] = true
			ids := map[int64]bool{}
			for _, a := range c.AccountIDs {
				if a <= 0 || ids[a] {
					return false
				}
				ids[a] = true
			}
		}
		seen = map[int64]bool{}
		for _, c := range o.clients.Contacts {
			if c.ID <= 0 || seen[c.ID] || !managerText(c.Name, 0, 512) {
				return false
			}
			seen[c.ID] = true
			ids := map[int64]bool{}
			for _, a := range c.CompanyIDs {
				if a <= 0 || ids[a] {
					return false
				}
				ids[a] = true
			}
		}
		return true
	}
	if o.metadata == nil || o.metadata.RequiredFieldsState != "unknown" {
		return false
	}
	if o.metadata.Employees != nil {
		users := map[string]bool{}
		for _, user := range *o.metadata.Employees {
			if !positiveCRMID(user.ID) || users[user.ID] || !managerText(user.Name, 0, 512) {
				return false
			}
			users[user.ID] = true
		}
	}
	seen := map[int64]bool{}
	for _, p := range o.metadata.Pipelines {
		if p.ID <= 0 || seen[p.ID] || p.AccountID != o.source.AccountID || !managerText(p.Name, 0, 512) {
			return false
		}
		seen[p.ID] = true
		ids := map[int64]bool{}
		for _, s := range p.Embedded.Statuses {
			if s.ID <= 0 || ids[s.ID] || s.PipelineID != p.ID || s.AccountID != o.source.AccountID || !managerText(s.Name, 0, 512) {
				return false
			}
			ids[s.ID] = true
		}
	}
	return true
}
func publishCompanyObservation(ctx context.Context, tx pgx.Tx, p companyJobPayload, job uuid.UUID, o companyObservation) (queue.Outcome, error) {
	var active companyActivation
	err := tx.QueryRow(ctx, `SELECT scope_id,epoch FROM platform_company.source_control WHERE singleton FOR UPDATE`).Scan(&active.Scope, &active.Epoch)
	if err != nil {
		return queue.Outcome{}, err
	}
	if active != p.companyActivation {
		return queue.Outcome{FailureCode: "source_scope_changed"}, nil
	}
	var current, linked *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT current_generation,current_job_id FROM platform_company.source_components WHERE scope_id=$1 AND component=$2 FOR UPDATE`, p.Scope, p.Component).Scan(&current, &linked)
	if err != nil {
		return queue.Outcome{}, err
	}
	if linked == nil || *linked != job {
		return queue.Outcome{FailureCode: "source_scope_changed"}, nil
	}
	outcome := queue.Outcome{}
	if o.code != "" {
		outcome.FailureCode = o.code
		outcome.Retry = true
		outcome.RetryAfter = 5 * time.Second
	}
	if o.retry != nil || o.code == "source_rate_limited" {
		if o.retry == nil {
			at := time.Now().Add(outcome.RetryAfter)
			o.retry = &at
		}
		outcome.RetryAfter = max(outcome.RetryAfter, time.Until(*o.retry))
		if _, err = tx.Exec(ctx, `UPDATE platform_company.source_control SET retry_not_before=GREATEST(retry_not_before,$1,statement_timestamp()+interval '5 seconds') WHERE singleton`, *o.retry); err != nil {
			return outcome, err
		}
	}
	if o.code == "" {
		var raw []byte
		if p.Component == companyClients {
			raw, _ = json.Marshal(o.clients)
		} else {
			raw, _ = json.Marshal(o.metadata)
		}
		hash := sha256.Sum256(raw)
		var existing uuid.UUID
		var existingHash []byte
		var latest *time.Time
		err = tx.QueryRow(ctx, `SELECT max(observed_at) FROM platform_company.generations WHERE scope_id=$1 AND component=$2`, p.Scope, p.Component).Scan(&latest)
		if err != nil {
			return outcome, err
		}
		if latest != nil && o.source.ObservedAt.Before(*latest) {
			return queue.Outcome{}, nil
		}
		err = tx.QueryRow(ctx, `SELECT id,content_hash FROM platform_company.generations WHERE scope_id=$1 AND component=$2 AND observed_at=$3`, p.Scope, p.Component, o.source.ObservedAt).Scan(&existing, &existingHash)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return outcome, err
		}
		if existing != uuid.Nil && string(existingHash) != string(hash[:]) {
			o.code = "source_inconsistent"
			outcome = queue.Outcome{FailureCode: o.code, Retry: true, RetryAfter: time.Minute}
		} else {
			generation := existing
			if generation == uuid.Nil {
				generation = uuid.New()
				var fields *string
				var employees *[]companycrm.WorkflowEmployee
				if o.metadata != nil {
					fields = &o.metadata.RequiredFieldsState
					employees = o.metadata.Employees
				}
				_, err = tx.Exec(ctx, `INSERT INTO platform_company.generations(id,scope_id,component,observed_at,fresh_until,source_state,content_hash,required_fields_state,employees) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, generation, p.Scope, p.Component, o.source.ObservedAt, o.source.FreshUntil, o.source.State, hash[:], fields, employees)
				if err != nil {
					return outcome, err
				}
				if err = insertCompanyContent(ctx, tx, generation, p.Scope, o); err != nil {
					return outcome, err
				}
			}
			_, err = tx.Exec(ctx, `UPDATE platform_company.source_components SET current_generation=$3 WHERE scope_id=$1 AND component=$2`, p.Scope, p.Component, generation)
			if err != nil {
				return outcome, err
			}
		}
	}
	state := o.source.State
	if o.code != "" && state != "disabled" {
		state = "error"
	}
	if state == "" {
		state = "error"
	}
	reportedCode := o.code
	if reportedCode == "" {
		reportedCode = safeCompanySourceCode(o.source.ErrorCode)
	}
	_, err = tx.Exec(ctx, `UPDATE platform_company.source_components SET attempt_state=$3,attempt_at=clock_timestamp(),error_code=NULLIF($4,''),next_refresh=clock_timestamp()+interval '1 minute' WHERE scope_id=$1 AND component=$2`, p.Scope, p.Component, state, reportedCode)
	return outcome, err
}
func insertCompanyContent(ctx context.Context, tx pgx.Tx, generation, scope uuid.UUID, o companyObservation) error {
	if o.clients == nil {
		pipelines := make([][]any, 0, len(o.metadata.Pipelines))
		var stages [][]any
		for _, p := range o.metadata.Pipelines {
			pipelines = append(pipelines, []any{generation, p.ID, p.Name})
			for _, s := range p.Embedded.Statuses {
				stages = append(stages, []any{generation, p.ID, s.ID, s.Name})
			}
		}
		if err := copyCompanyRows(ctx, tx, "workflow_pipelines", []string{"generation_id", "pipeline_id", "name"}, pipelines); err != nil {
			return err
		}
		return copyCompanyRows(ctx, tx, "workflow_stages", []string{"generation_id", "pipeline_id", "stage_id", "name"}, stages)
	}
	crmIDs := make([]int64, 0, len(o.clients.Companies))
	newIDs := make([]uuid.UUID, 0, len(o.clients.Companies))
	for _, company := range o.clients.Companies {
		crmIDs = append(crmIDs, company.CompanyID)
		newIDs = append(newIDs, uuid.New())
	}
	_, err := tx.Exec(ctx, `INSERT INTO platform_company.company_identities(id,source_account_id,crm_company_id)
 SELECT item.id,$1,item.crm_id FROM unnest($2::uuid[],$3::bigint[]) AS item(id,crm_id)
 ON CONFLICT(source_account_id,crm_company_id) DO NOTHING`, o.source.AccountID, newIDs, crmIDs)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT crm_company_id,id FROM platform_company.company_identities WHERE source_account_id=$1 AND crm_company_id=ANY($2)`, o.source.AccountID, crmIDs)
	if err != nil {
		return err
	}
	type identity struct {
		crm int64
		id  uuid.UUID
	}
	identities, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (identity, error) {
		var i identity
		err := r.Scan(&i.crm, &i.id)
		return i, err
	})
	if err != nil {
		return err
	}
	ids := make(map[int64]uuid.UUID, len(identities))
	for _, id := range identities {
		ids[id.crm] = id.id
	}
	companies := make([][]any, 0, len(o.clients.Companies))
	var claims [][]any
	for _, c := range o.clients.Companies {
		id := ids[c.CompanyID]
		companies = append(companies, []any{generation, id, c.CompanyID, c.Name, c.Resolution, scope, o.source.AccountID})
		for _, account := range c.AccountIDs {
			claims = append(claims, []any{generation, id, account})
		}
	}
	if err := copyCompanyRows(ctx, tx, "company_rows", []string{"generation_id", "company_id", "crm_company_id", "name", "source_resolution", "scope_id", "source_account_id"}, companies); err != nil {
		return err
	}
	if err := copyCompanyRows(ctx, tx, "company_account_claims", []string{"generation_id", "company_id", "account_id"}, claims); err != nil {
		return err
	}
	contacts := make([][]any, 0, len(o.clients.Contacts))
	var links [][]any
	for _, c := range o.clients.Contacts {
		contacts = append(contacts, []any{generation, c.ID, c.Name})
		for _, id := range c.CompanyIDs {
			links = append(links, []any{generation, c.ID, id, commandUUID(ids[id])})
		}
	}
	if err := copyCompanyRows(ctx, tx, "contact_rows", []string{"generation_id", "crm_contact_id", "name"}, contacts); err != nil {
		return err
	}
	return copyCompanyRows(ctx, tx, "contact_company_links", []string{"generation_id", "crm_contact_id", "crm_company_id", "company_id"}, links)
}
func copyCompanyRows(ctx context.Context, tx pgx.Tx, table string, columns []string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"platform_company", table}, columns, pgx.CopyFromRows(rows))
	return err
}

func safeCompanySourceCode(code string) string {
	if len(code) > 64 {
		return "source_unavailable"
	}
	for _, c := range code {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return "source_unavailable"
		}
	}
	return code
}
