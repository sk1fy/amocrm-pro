package productplatform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

const companyWorkflowCommandKind = "product.company_workflow.set"

type companyProjectionID uuid.UUID

func (id companyProjectionID) MarshalJSON() ([]byte, error) { return json.Marshal(uuid.UUID(id)) }

func (id *companyProjectionID) UnmarshalJSON(b []byte) error {
	var v uuid.UUID
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*id = companyProjectionID(v)
	return nil
}

type companySource struct {
	Account    *string    `json:"source_account_id"`
	Binding    *string    `json:"binding_revision"`
	State      string     `json:"state"`
	Complete   bool       `json:"complete"`
	ObservedAt *time.Time `json:"observed_at"`
	FreshUntil *time.Time `json:"fresh_until"`
	ErrorCode  *string    `json:"error_code"`
	RetryAt    *time.Time `json:"retry_at"`
}
type companyProjection struct {
	activation companyActivation
	generation *uuid.UUID
	source     companySource
}

func readCompanyProjection(ctx context.Context, tx pgx.Tx, part companyComponent, pinned *uuid.UUID) (companyProjection, error) {
	p := companyProjection{source: companySource{State: "disabled"}}
	err := tx.QueryRow(ctx, `SELECT c.scope_id,c.epoch,s.account_id::text,s.binding_revision,c.retry_not_before,x.current_generation,x.attempt_state,x.error_code
 FROM platform_company.source_control c JOIN platform_company.source_scopes s ON s.id=c.scope_id JOIN platform_company.source_components x ON x.scope_id=c.scope_id AND x.component=$1`, part).
		Scan(&p.activation.Scope, &p.activation.Epoch, &p.source.Account, &p.source.Binding, &p.source.RetryAt, &p.generation, &p.source.State, &p.source.ErrorCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if pinned != nil {
		p.generation = pinned
	}
	if p.generation != nil {
		var state string
		err = tx.QueryRow(ctx, `SELECT observed_at,fresh_until,source_state FROM platform_company.generations WHERE id=$1 AND scope_id=$2 AND component=$3`, *p.generation, p.activation.Scope, part).Scan(&p.source.ObservedAt, &p.source.FreshUntil, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return p, errManagerCursor
		}
		if err != nil {
			return p, err
		}
		p.source.Complete = true
		if p.source.State == "fresh" {
			p.source.State = state
			if !p.source.FreshUntil.After(time.Now()) {
				p.source.State = "stale"
			}
		}
	}
	return p, nil
}

type workflowStage struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type workflowPipeline struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Stages []workflowStage `json:"stages"`
}
type companyWorkflowMetadata struct {
	Generation          *uuid.UUID         `json:"generation_id"`
	Source              companySource      `json:"source"`
	Pipelines           []workflowPipeline `json:"pipelines"`
	RequiredFieldsState string             `json:"required_fields_state"`
	WriteReady          bool               `json:"write_ready"`
}

func (s managerStore) companyWorkflowMetadata(ctx context.Context) (companyWorkflowMetadata, error) {
	result := companyWorkflowMetadata{RequiredFieldsState: "unknown"}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p, err := readCompanyProjection(ctx, tx, companyMetadata, nil)
	if err != nil {
		return result, err
	}
	result.Generation, result.Source = p.generation, p.source
	if p.generation != nil {
		rows, err := tx.Query(ctx, `SELECT p.pipeline_id::text,p.name,COALESCE((SELECT jsonb_agg(jsonb_build_object('id',s.stage_id::text,'name',s.name) ORDER BY s.stage_id) FROM platform_company.workflow_stages s WHERE s.generation_id=p.generation_id AND s.pipeline_id=p.pipeline_id),'[]') FROM platform_company.workflow_pipelines p WHERE p.generation_id=$1 ORDER BY p.pipeline_id`, *p.generation)
		if err != nil {
			return result, err
		}
		result.Pipelines, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (workflowPipeline, error) {
			var p workflowPipeline
			err := r.Scan(&p.ID, &p.Name, &p.Stages)
			return p, err
		})
		if err != nil {
			return result, err
		}
	}
	return result, tx.Commit(ctx)
}

type workflowConfiguration struct {
	Revision    int64     `json:"revision"`
	Account     string    `json:"source_account_id"`
	Binding     string    `json:"binding_revision"`
	Generation  uuid.UUID `json:"metadata_generation_id"`
	Pipeline    string    `json:"pipeline_id"`
	Stage       string    `json:"initial_stage_id"`
	ValidatedAt time.Time `json:"validated_at"`
}
type productCompanyWorkflow struct {
	Product       uuid.UUID              `json:"product_id"`
	Version       int64                  `json:"version"`
	Configuration *workflowConfiguration `json:"configuration"`
	WriteReady    bool                   `json:"write_ready"`
}

func readProductCompanyWorkflow(ctx context.Context, tx pgx.Tx, id uuid.UUID) (productCompanyWorkflow, error) {
	result := productCompanyWorkflow{Product: id, Version: 1}
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_lifecycle.products WHERE id=$1 AND catalog_version IS NOT NULL)`, id).Scan(&exists)
	if err != nil {
		return result, err
	}
	if !exists {
		return result, errManagerNotFound
	}
	var c workflowConfiguration
	err = tx.QueryRow(ctx, `SELECT h.version,r.revision,s.account_id::text,s.binding_revision,r.metadata_generation,r.pipeline_id::text,r.stage_id::text,r.validated_at
 FROM platform_company.product_workflow_heads h JOIN platform_company.product_workflow_revisions r ON (r.product_id,r.revision)=(h.product_id,h.revision) JOIN platform_company.source_scopes s ON s.id=r.scope_id WHERE h.product_id=$1`, id).Scan(&result.Version, &c.Revision, &c.Account, &c.Binding, &c.Generation, &c.Pipeline, &c.Stage, &c.ValidatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err == nil {
		result.Configuration = &c
	}
	return result, err
}

type companyWorkflowInput struct {
	Account    string    `json:"source_account_id"`
	Binding    string    `json:"binding_revision"`
	Generation uuid.UUID `json:"metadata_generation_id"`
	Pipeline   string    `json:"pipeline_id"`
	Stage      string    `json:"initial_stage_id"`
}

func positiveCRMID(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == s
}

type companyMetadataUnavailable struct{ retry *time.Time }

func (e *companyMetadataUnavailable) Error() string { return "company metadata unavailable" }
func saveCompanyWorkflow(ctx context.Context, tx pgx.Tx, cmd managerCommand, in companyWorkflowInput) (managerOutcome, error) {
	var scope uuid.UUID
	var account, binding string
	var retry *time.Time
	err := tx.QueryRow(ctx, `SELECT c.scope_id,s.account_id::text,s.binding_revision,c.retry_not_before FROM platform_company.source_control c JOIN platform_company.source_scopes s ON s.id=c.scope_id FOR UPDATE OF c`).Scan(&scope, &account, &binding, &retry)
	if errors.Is(err, pgx.ErrNoRows) {
		return managerOutcome{}, &companyMetadataUnavailable{}
	}
	if err != nil {
		return managerOutcome{}, err
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM platform_lifecycle.products WHERE id=$1 AND catalog_version IS NOT NULL FOR NO KEY UPDATE`, cmd.Product).Scan(&id)
	if err != nil {
		return managerOutcome{}, accountReadError(err)
	}
	current, err := readProductCompanyWorkflow(ctx, tx, cmd.Product)
	if err != nil {
		return managerOutcome{}, err
	}
	if current.Version != cmd.Version {
		return managerVersionConflict(cmd, current.Version)
	}
	reject := func(status int, code string) (managerOutcome, error) {
		return managerMutationResponse(status, 0, managerProblem(status, code, cmd.RequestID, 0, 0))
	}
	if in.Account != account || in.Binding != binding {
		return reject(409, "company_source_conflict")
	}
	p, err := readCompanyProjection(ctx, tx, companyMetadata, nil)
	if err != nil {
		return managerOutcome{}, err
	}
	if p.generation == nil || !p.source.Complete || p.source.State != "fresh" || p.source.FreshUntil == nil || !p.source.FreshUntil.After(time.Now()) {
		return managerOutcome{}, &companyMetadataUnavailable{retry}
	}
	if *p.generation != in.Generation {
		return reject(409, "company_metadata_conflict")
	}
	var member bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_company.workflow_stages WHERE generation_id=$1 AND pipeline_id=$2 AND stage_id=$3)`, in.Generation, in.Pipeline, in.Stage).Scan(&member)
	if err != nil {
		return managerOutcome{}, err
	}
	if !member {
		return reject(422, "company_stage_mismatch")
	}
	if current.Version >= wire.MaxPosition {
		return reject(409, "version_exhausted")
	}
	revision := current.Version
	_, err = tx.Exec(ctx, `INSERT INTO platform_company.product_workflow_revisions(product_id,revision,scope_id,metadata_generation,pipeline_id,stage_id,command_id,actor_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, cmd.Product, revision, scope, in.Generation, in.Pipeline, in.Stage, cmd.ID, cmd.Subject)
	if err != nil {
		return managerOutcome{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO platform_company.product_workflow_heads VALUES($1,$2,$3) ON CONFLICT(product_id) DO UPDATE SET version=EXCLUDED.version,revision=EXCLUDED.revision`, cmd.Product, current.Version+1, revision)
	if err != nil {
		return managerOutcome{}, err
	}
	result, err := readProductCompanyWorkflow(ctx, tx, cmd.Product)
	if err != nil {
		return managerOutcome{}, err
	}
	return managerMutationResponse(http.StatusOK, result.Version, result)
}
func registerManagerCompanies(mux *http.ServeMux, m managerHTTP) {
	mux.HandleFunc("GET "+managerPath+"companies", managerAuthorized("clients.read", false, m.listCompanies))
	mux.HandleFunc("GET "+managerPath+"companies/{id}", managerAuthorized("clients.read", false, m.getCompany))
	mux.HandleFunc("GET "+managerPath+"company-workflow-metadata", managerAuthorized("catalog.manage", false, m.getCompanyWorkflowMetadata))
	mux.HandleFunc("GET "+managerPath+"products/{id}/company-workflow", managerAuthorized("catalog.manage", false, m.getProductCompanyWorkflow))
	mux.HandleFunc("PUT "+managerPath+"products/{id}/company-workflow", managerAuthorized("catalog.manage", true, m.setProductCompanyWorkflow))
}
func (m managerHTTP) getCompanyWorkflowMetadata(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	if r.URL.RawQuery != "" {
		writeManagerError(w, 400, "validation_failed", env.RequestID, 0)
		return
	}
	result, err := m.store.companyWorkflowMetadata(r.Context())
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	writeLifecycleJSON(w, 200, result)
}
func (m managerHTTP) getProductCompanyWorkflow(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, err := managerUUID(r.PathValue("id"))
	if err != nil || r.URL.RawQuery != "" {
		writeManagerError(w, 400, "validation_failed", env.RequestID, 0)
		return
	}
	tx, err := m.store.pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	result, err := readProductCompanyWorkflow(r.Context(), tx, id)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	w.Header().Set("ETag", managerETag(result.Version))
	writeLifecycleJSON(w, 200, result)
}
func (m managerHTTP) setProductCompanyWorkflow(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, e1 := managerUUID(r.PathValue("id"))
	key, e2 := managerUUID(singleManagerHeader(r.Header, "Idempotency-Key"))
	version, e3 := managerVersion(singleManagerHeader(r.Header, "If-Match"))
	media, _, _ := mime.ParseMediaType(singleManagerHeader(r.Header, "Content-Type"))
	if e1 != nil || e2 != nil || e3 != nil || media != "application/json" || r.URL.RawQuery != "" {
		writeManagerError(w, 400, "validation_failed", env.RequestID, 0)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	var in companyWorkflowInput
	if err != nil || readManagerObject(raw, map[string]any{"source_account_id": &in.Account, "binding_revision": &in.Binding, "metadata_generation_id": &in.Generation, "pipeline_id": &in.Pipeline, "initial_stage_id": &in.Stage}) != nil || !positiveCRMID(in.Account) || !positiveCRMID(in.Pipeline) || !positiveCRMID(in.Stage) || !managerText(in.Binding, 1, 256) || in.Generation == uuid.Nil {
		writeManagerError(w, 400, "validation_failed", env.RequestID, 0)
		return
	}
	cmd := managerCommand{ID: env.Command, Key: key, Subject: env.Actor.Subject, RequestID: env.RequestID, Product: id, Kind: companyWorkflowCommandKind, Version: version}
	outcome, err := m.store.mutate(r.Context(), cmd, in, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		return saveCompanyWorkflow(ctx, tx, cmd, in)
	})
	var unavailable *companyMetadataUnavailable
	if errors.As(err, &unavailable) {
		if unavailable.retry != nil {
			seconds := int64(time.Until(*unavailable.retry).Seconds() + 1)
			if seconds > 0 {
				w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
			}
		}
		problem := managerProblem(503, "company_metadata_unavailable", env.RequestID, 0, 0)
		problem.Error.Retryable = true
		writeLifecycleJSON(w, 503, problem)
		return
	}
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	writeManagerOutcome(w, outcome)
}
