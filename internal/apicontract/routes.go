package apicontract

import "net/http"

type Route struct {
	Method string
	Path   string
}

var (
	Live                          = Route{Method: http.MethodGet, Path: "/live"}
	Ready                         = Route{Method: http.MethodGet, Path: "/ready"}
	Metrics                       = Route{Method: http.MethodGet, Path: "/metrics"}
	OAuthStart                    = Route{Method: http.MethodGet, Path: "/oauth/amocrm/start"}
	OAuthCallback                 = Route{Method: http.MethodGet, Path: "/oauth/amocrm/callback"}
	WebhookReceive                = Route{Method: http.MethodPost, Path: "/hooks/amocrm/v1/{webhookKey}"}
	WidgetBootstrap               = Route{Method: http.MethodGet, Path: "/api/v1/widget/bootstrap"}
	WidgetPing                    = Route{Method: http.MethodPost, Path: "/api/v1/widget/actions/ping"}
	WidgetLeadSetStatus           = Route{Method: http.MethodPost, Path: "/api/v1/widget/actions/leads/set-status"}
	WidgetLeadStatusRuleConfigure = Route{Method: http.MethodPost, Path: "/api/v1/widget/workflow-rules/lead-status/configure"}
	WidgetJob                     = Route{Method: http.MethodGet, Path: "/api/v1/widget/jobs/{jobID}"}
	ActivityEvent                 = Route{Method: http.MethodGet, Path: "/api/v1/widget/activity/events/{eventID}"}
	ActivityPanel                 = Route{Method: http.MethodGet, Path: "/api/v1/widget/activity/panel"}
	ActivityStatus                = Route{Method: http.MethodGet, Path: "/api/v1/widget/activity/status"}
	ActivitySettings              = Route{Method: http.MethodGet, Path: "/api/v1/widget/activity/settings"}
	ActivityConfigure             = Route{Method: http.MethodPost, Path: "/api/v1/widget/activity/settings"}
	ActivitySync                  = Route{Method: http.MethodPost, Path: "/api/v1/widget/activity/sync"}
	ActivityOperation             = Route{Method: http.MethodGet, Path: "/api/v1/widget/activity/operations/{operationID}"}
	ActivityViewPanel             = Route{Method: http.MethodGet, Path: "/api/v1/activity/view/panel"}
	ActivityViewTimeline          = Route{Method: http.MethodGet, Path: "/api/v1/activity/view/timeline"}
	ActivityViewEmployee          = Route{Method: http.MethodGet, Path: "/api/v1/activity/view/employees/{employeeId}"}
	ActivityViewEvent             = Route{Method: http.MethodGet, Path: "/api/v1/activity/view/events/{eventId}"}
	ActivityManagedPanels         = Route{Method: http.MethodGet, Path: "/api/v1/activity/panels"}
	ActivityManagedPanelCreate    = Route{Method: http.MethodPost, Path: "/api/v1/activity/panels"}
	ActivityManagedPanel          = Route{Method: http.MethodGet, Path: "/api/v1/activity/panels/{panelId}"}
	ActivityManagedPanelPatch     = Route{Method: http.MethodPatch, Path: "/api/v1/activity/panels/{panelId}"}
	ActivityManagedShareLink      = Route{Method: http.MethodPost, Path: "/api/v1/activity/panels/{panelId}/share-link"}
	ActivityManagedEmployees      = Route{Method: http.MethodGet, Path: "/api/v1/activity/employees"}

	Routes = []Route{
		Live,
		OAuthStart,
		OAuthCallback,
		WebhookReceive,
		WidgetBootstrap,
		WidgetPing,
		WidgetLeadSetStatus,
		WidgetLeadStatusRuleConfigure,
		WidgetJob,
		ActivityEvent,
		ActivityPanel,
		ActivityStatus,
		ActivitySettings,
		ActivityConfigure,
		ActivitySync,
		ActivityOperation,
		ActivityViewPanel,
		ActivityViewTimeline,
		ActivityViewEmployee,
		ActivityViewEvent,
		ActivityManagedPanels,
		ActivityManagedPanelCreate,
		ActivityManagedPanel,
		ActivityManagedPanelPatch,
		ActivityManagedShareLink,
		ActivityManagedEmployees,
	}

	ManagementRoutes = []Route{
		Live,
		Ready,
		Metrics,
	}

	AdminBackend                = Route{Method: http.MethodGet, Path: "/admin/v1/backend"}
	AdminAccounts               = Route{Method: http.MethodGet, Path: "/admin/v1/accounts"}
	AdminAccount                = Route{Method: http.MethodGet, Path: "/admin/v1/accounts/{account_id}"}
	AdminInstallations          = Route{Method: http.MethodGet, Path: "/admin/v1/installations"}
	AdminInstallation           = Route{Method: http.MethodGet, Path: "/admin/v1/installations/{id}"}
	AdminInstallationJobs       = Route{Method: http.MethodGet, Path: "/admin/v1/installations/{id}/jobs"}
	AdminInstallationAudit      = Route{Method: http.MethodGet, Path: "/admin/v1/installations/{id}/audit"}
	AdminInstallationDeliveries = Route{Method: http.MethodGet, Path: "/admin/v1/installations/{id}/activity/deliveries"}
	AdminIntegrations           = Route{Method: http.MethodGet, Path: "/admin/v1/integrations"}
	AdminIntegration            = Route{Method: http.MethodGet, Path: "/admin/v1/integrations/{id}"}
	AdminJobs                   = Route{Method: http.MethodGet, Path: "/admin/v1/jobs"}
	AdminJobsSummary            = Route{Method: http.MethodGet, Path: "/admin/v1/jobs/summary"}
	AdminJob                    = Route{Method: http.MethodGet, Path: "/admin/v1/jobs/{id}"}
	AdminAudit                  = Route{Method: http.MethodGet, Path: "/admin/v1/audit"}
	AdminCommands               = Route{Method: http.MethodPost, Path: "/admin/v1/commands"}
	AdminCommand                = Route{Method: http.MethodGet, Path: "/admin/v1/commands/{id}"}
	AdminCommandRoutes          = []Route{AdminCommands, AdminCommand}

	AdminActivitySettings  = Route{Method: http.MethodGet, Path: "/admin/v1/installations/{id}/activity/settings"}
	AdminActivityStatus    = Route{Method: http.MethodGet, Path: "/admin/v1/installations/{id}/activity/status"}
	AdminActivityOperation = Route{Method: http.MethodGet, Path: "/admin/v1/installations/{id}/activity/operations/{operation_id}"}
	AdminActivityPanels    = Route{Method: http.MethodGet, Path: "/admin/v1/installations/{id}/activity/panels"}
	AdminActivityPanel     = Route{Method: http.MethodGet, Path: "/admin/v1/installations/{id}/activity/panels/{panel_id}"}
	AdminActivityEmployees = Route{Method: http.MethodGet, Path: "/admin/v1/installations/{id}/activity/employees"}
	AdminLeadStatusRules   = Route{Method: http.MethodGet, Path: "/admin/v1/installations/{id}/lead-status/rules"}
	AdminLeadStatusRuns    = Route{Method: http.MethodGet, Path: "/admin/v1/installations/{id}/lead-status/runs"}
	AdminStats             = Route{Method: http.MethodGet, Path: "/admin/v1/stats"}
	AdminStatsAccounts     = Route{Method: http.MethodGet, Path: "/admin/v1/stats/accounts"}

	AdminRoutes = []Route{
		AdminBackend,
		AdminAccounts,
		AdminAccount,
		AdminInstallations,
		AdminInstallation,
		AdminInstallationJobs,
		AdminInstallationAudit,
		AdminInstallationDeliveries,
		AdminActivitySettings,
		AdminActivityStatus,
		AdminActivityOperation,
		AdminActivityPanels,
		AdminActivityPanel,
		AdminActivityEmployees,
		AdminLeadStatusRules,
		AdminLeadStatusRuns,
		AdminIntegrations,
		AdminIntegration,
		AdminJobs,
		AdminJobsSummary,
		AdminJob,
		AdminAudit,
		AdminStats,
		AdminStatsAccounts,
	}
)
