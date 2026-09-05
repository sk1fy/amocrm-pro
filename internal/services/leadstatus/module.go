// Package leadstatus owns the lead-status product: widget commands, workflow
// rules, webhook routing, durable jobs and typed public results. Wire modules
// explicitly at the API/worker composition roots; platform packages never
// import a product module.
package leadstatus

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
)

const Code = services.LeadStatus

type Module struct {
	actions   *ActionStore
	execution *ExecutionStore
	rules     *RuleStore
	workflows *WorkflowStore
}

func NewModule(pool *pgxpool.Pool, jobStore *jobs.Store) *Module {
	return &Module{actions: NewActionStore(pool, jobStore), execution: NewExecutionStore(pool), rules: NewRuleStore(pool), workflows: NewWorkflowStore(pool)}
}

// Routes returns a fresh copy of this module's public command contracts.
func (m *Module) Routes() []apicontract.Route {
	return []apicontract.Route{apicontract.WidgetLeadSetStatus, apicontract.WidgetLeadStatusRuleConfigure}
}

func (m *Module) RegisterHTTP(router chi.Router, middleware func(http.Handler) http.Handler) {
	handler := NewHandler(m.actions)
	router.Method(apicontract.WidgetLeadSetStatus.Method, apicontract.WidgetLeadSetStatus.Path, middleware(http.HandlerFunc(handler.LeadSetStatus)))
	router.Method(apicontract.WidgetLeadStatusRuleConfigure.Method, apicontract.WidgetLeadStatusRuleConfigure.Path, middleware(http.HandlerFunc(handler.ConfigureLeadStatusRule)))
	for _, route := range m.Routes() {
		router.Method(http.MethodOptions, route.Path, middleware(http.NotFoundHandler()))
	}

}

func (m *Module) RegisterResults(handler *widgetapi.Handler) { RegisterResults(handler) }

func (m *Module) RegisterJobs(handlers map[string]jobs.Handler, observers map[string]jobs.FailureObserver, api LeadStatusAPI) {
	product := map[string]jobs.Handler{
		LeadSetStatusJobType:           LeadSetStatusJobHandler(m.execution, api),
		LeadStatusRuleConfigureJobType: LeadStatusRuleConfigureJobHandler(m.execution, m.rules, api),
		LeadStatusTransitionJobType:    LeadStatusTransitionJobHandler(m.workflows, m.execution, api),
	}
	for jobType, handler := range product {
		if _, exists := handlers[jobType]; exists {
			panic("duplicate product job handler: " + jobType)
		}
		handlers[jobType] = handler
	}
	if _, exists := observers[LeadStatusTransitionJobType]; exists {
		panic("duplicate product failure observer")
	}
	observers[LeadStatusTransitionJobType] = m.workflows.RecordJobFailure
}

type EventRegistrar interface {
	RegisterEventRouter(string, string, services.EventRouter)
}

func (m *Module) RegisterEvents(registrar EventRegistrar) {
	registrar.RegisterEventRouter("leads", "status", func(ctx context.Context, tx pgx.Tx, event services.Event) (services.EventRoute, error) {
		route, err := m.workflows.routeLeadStatusEvent(ctx, tx, event)
		route.Workflow = "lead_status_transition"
		return route, err
	})
}
