package apicontract

import "net/http"

type Route struct {
	Method string
	Path   string
}

var (
	Live                = Route{Method: http.MethodGet, Path: "/live"}
	Ready               = Route{Method: http.MethodGet, Path: "/ready"}
	Metrics             = Route{Method: http.MethodGet, Path: "/metrics"}
	OAuthStart          = Route{Method: http.MethodGet, Path: "/oauth/amocrm/start"}
	OAuthCallback       = Route{Method: http.MethodGet, Path: "/oauth/amocrm/callback"}
	WebhookReceive      = Route{Method: http.MethodPost, Path: "/hooks/amocrm/v1/{webhookKey}"}
	WidgetBootstrap     = Route{Method: http.MethodGet, Path: "/api/v1/widget/bootstrap"}
	WidgetPing          = Route{Method: http.MethodPost, Path: "/api/v1/widget/actions/ping"}
	WidgetLeadSetStatus = Route{Method: http.MethodPost, Path: "/api/v1/widget/actions/leads/set-status"}
	WidgetJob           = Route{Method: http.MethodGet, Path: "/api/v1/widget/jobs/{jobID}"}

	Routes = []Route{
		Live,
		Ready,
		Metrics,
		OAuthStart,
		OAuthCallback,
		WebhookReceive,
		WidgetBootstrap,
		WidgetPing,
		WidgetLeadSetStatus,
		WidgetJob,
	}
)
