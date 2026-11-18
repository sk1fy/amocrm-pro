package companycrm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type resolverFunc func(context.Context) (Binding, error)

func (f resolverFunc) Resolve(ctx context.Context) (Binding, error) { return f(ctx) }

type fakeCRM struct {
	companies             []amocrm.CompanyCRMCompany
	contacts              []amocrm.CompanyCRMContact
	fields                []amocrm.CompanyCRMField
	pipelines             []amocrm.CompanyCRMPipeline
	verifyErr, errorStaff error
	users                 []amocrm.CompanyCRMUser
	calls, verifies       int
	during                func()
}

func (f *fakeCRM) VerifyCompanyCRMAccount(context.Context, uuid.UUID, int64) error {
	f.verifies++
	return f.verifyErr
}
func (f *fakeCRM) CompanyCRMStaff(context.Context, uuid.UUID) ([]amocrm.CompanyCRMUser, error) {
	f.calls++
	if f.during != nil {
		f.during()
	}
	return f.users, f.errorStaff
}
func (f *fakeCRM) CompanyCRMCompanies(context.Context, uuid.UUID, int64) ([]amocrm.CompanyCRMCompany, error) {
	return f.companies, nil
}
func (f *fakeCRM) CompanyCRMContacts(context.Context, uuid.UUID, int64) ([]amocrm.CompanyCRMContact, error) {
	return f.contacts, nil
}
func (f *fakeCRM) CompanyCRMFields(context.Context, uuid.UUID, string) ([]amocrm.CompanyCRMField, error) {
	return f.fields, nil
}
func (f *fakeCRM) CompanyCRMPipelines(context.Context, uuid.UUID, int64) ([]amocrm.CompanyCRMPipeline, error) {
	return f.pipelines, nil
}
func binding() Binding {
	return Binding{AccountID: 99, InstallationID: uuid.New(), IntegrationID: uuid.New(), Revision: "r1"}
}
func TestStaffUnknownAndFailureRetainsFullSnapshot(t *testing.T) {
	b := binding()
	now := time.Now()
	f := &fakeCRM{users: []amocrm.CompanyCRMUser{{ID: 7, Name: "Staff"}}}
	s := &Service{Resolver: resolverFunc(func(context.Context) (Binding, error) { return b, nil }), CRM: f, now: func() time.Time { return now }}
	got := s.Staff(t.Context())
	if got.Source.State != "fresh" || !got.Source.Complete || (*got.Data)[0].Activity != "unknown" {
		t.Fatalf("response=%+v", got)
	}
	now = now.Add(time.Minute)
	f.errorStaff = amocrm.ErrIncompleteResponse
	stale := s.Staff(t.Context())
	if stale.Source.State != "stale" || !stale.Source.Complete || stale.Data != got.Data || !stale.Source.ObservedAt.Equal(*got.Source.ObservedAt) {
		t.Fatalf("stale=%+v", stale)
	}
}
func TestWrongAccountStopsBeforeStaff(t *testing.T) {
	f := &fakeCRM{verifyErr: errors.New("wrong account")}
	b := binding()
	s := &Service{Resolver: resolverFunc(func(context.Context) (Binding, error) { return b, nil }), CRM: f}
	got := s.Staff(t.Context())
	if got.Data != nil || got.Source.Complete || f.calls != 0 {
		t.Fatalf("wrong account response=%+v calls=%d", got, f.calls)
	}
}
func TestBindingChangeDoesNotPublishOrRelabelOldSnapshot(t *testing.T) {
	b := binding()
	now := time.Now()
	f := &fakeCRM{users: []amocrm.CompanyCRMUser{{ID: 7}}}
	s := &Service{Resolver: resolverFunc(func(context.Context) (Binding, error) { return b, nil }), CRM: f, now: func() time.Time { return now }}
	first := s.Staff(t.Context())
	now = now.Add(time.Minute)
	f.users = []amocrm.CompanyCRMUser{{ID: 8}}
	f.during = func() { b.Revision = "r2"; b.AccountID = 100 }
	got := s.Staff(t.Context())
	if got.Source.State != "stale" || got.Source.BindingRevision != "r1" || got.Source.AccountID != 99 || got.Data != first.Data || (*got.Data)[0].ID != 7 {
		t.Fatalf("changed=%+v", got)
	}
}
func TestRateLimitPreservesRetryAndDoesNotRefetch(t *testing.T) {
	b := binding()
	now := time.Now()
	f := &fakeCRM{errorStaff: &amocrm.APIError{Kind: amocrm.ErrorRateLimited, RetryAfter: 90 * time.Second}}
	s := &Service{Resolver: resolverFunc(func(context.Context) (Binding, error) { return b, nil }), CRM: f, now: func() time.Time { return now }}
	got := s.Staff(t.Context())
	if got.Source.RetryAfter != 90 || got.Data != nil {
		t.Fatalf("rate limit=%+v", got)
	}
	now = now.Add(time.Second)
	s.Staff(t.Context())
	if f.calls != 1 {
		t.Fatalf("refetched during cooldown calls=%d", f.calls)
	}
}
func TestCancelledWaitIsBounded(t *testing.T) {
	b := binding()
	f := &fakeCRM{}
	s := &Service{Resolver: resolverFunc(func(context.Context) (Binding, error) { return b, nil }), CRM: f}
	s.init.Do(func() { s.gate = make(chan struct{}, 1) })
	s.gate <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got := s.Staff(ctx)
	if got.Source.ErrorCode != "deadline_exceeded" {
		t.Fatal(got)
	}
}

func TestClientsKeepStableRelationsAndExplicitAmbiguity(t *testing.T) {
	b := binding()
	b.AccountFieldID = 9
	f := &fakeCRM{fields: []amocrm.CompanyCRMField{{ID: 9, Type: "numeric"}}}
	if err := json.Unmarshal([]byte(`[
 {"id":1,"name":"Same","custom_fields_values":[{"field_id":9,"field_type":"numeric","values":[{"value":"100"},{"value":"101"}]}]},
 {"id":2,"name":"Same","custom_fields_values":[{"field_id":9,"field_type":"numeric","values":[{"value":"100"}]}]},
 {"id":3,"name":"Other"},
 {"id":4,"name":"No guessing","custom_fields_values":[{"field_id":9,"field_type":"numeric","values":[{"value":"name@example.test"}]}]}
 ]`), &f.companies); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`[{"id":7,"name":"Contact","_embedded":{"companies":[{"id":1}]}}]`), &f.contacts); err != nil {
		t.Fatal(err)
	}
	s := &Service{Resolver: resolverFunc(func(context.Context) (Binding, error) { return b, nil }), CRM: f}
	got := s.Clients(t.Context())
	if got.Data == nil {
		t.Fatal(got)
	}
	companies := got.Data.Companies
	if len(companies) != 4 || companies[0].CompanyID != 1 || len(companies[0].AccountIDs) != 2 || companies[0].Resolution != "multiple" || companies[1].Resolution != "multiple" || companies[2].Resolution != "zero" || companies[3].Resolution != "unknown" || got.Data.Contacts[0].ID != 7 || got.Data.Contacts[0].CompanyIDs[0] != 1 {
		t.Fatalf("clients=%+v", got.Data)
	}
}
func TestWorkflowChecksMembershipWithoutClaimingWriteReadiness(t *testing.T) {
	b := binding()
	f := &fakeCRM{}
	if err := json.Unmarshal([]byte(`[{"id":10,"_embedded":{"statuses":[{"id":20,"pipeline_id":10}]}}]`), &f.pipelines); err != nil {
		t.Fatal(err)
	}
	s := &Service{Resolver: resolverFunc(func(context.Context) (Binding, error) { return b, nil }), CRM: f}
	good := s.ValidateWorkflow(t.Context(), WorkflowRequest{BindingRevision: b.Revision, PipelineID: 10, StageID: 20})
	if !good.Valid || good.WriteReady || good.Scope != "pipeline_stage" {
		t.Fatal(good)
	}
	bad := s.ValidateWorkflow(t.Context(), WorkflowRequest{BindingRevision: b.Revision, PipelineID: 11, StageID: 20})
	if bad.Valid || bad.Code != "invalid_stage" {
		t.Fatal(bad)
	}
	mismatch := s.ValidateWorkflow(t.Context(), WorkflowRequest{BindingRevision: "old", PipelineID: 10, StageID: 20})
	if mismatch.Valid || mismatch.Code != "binding_mismatch" {
		t.Fatal(mismatch)
	}
}
