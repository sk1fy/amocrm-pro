package services

import "github.com/sk1fy/amocrm-pro/internal/apicontract"

// Component describes a statically registered service. Version is the wire
// contract version; ProductVersion is the feature release (Activity v0).
// A product capability and a transport contract version are separate concepts.
type Component struct {
	Code           string              `json:"service_code"`
	Version        string              `json:"contract_version"`
	ProductVersion string              `json:"product_version,omitempty"`
	MigrationOwner string              `json:"migration_owner"`
	Dependencies   []string            `json:"dependencies"`
	Modes          []string            `json:"supported_modes"`
	Requirement    string              `json:"requirement"`
	MaxConnections int                 `json:"default_max_connections"`
	Concurrency    int                 `json:"default_workers"`
	Health         string              `json:"health"`
	HTTPRoutes     []apicontract.Route `json:"http_routes,omitempty"`
	RPCs           []string            `json:"rpcs"`
}

func Components() []Component {
	components := []Component{
		{Code: Activity, Version: "v1", MigrationOwner: "activity", Dependencies: []string{CRMEvents, "gateway", "core-policy"}, Modes: []string{"embedded", "grpc"}, Requirement: "activity capability and installation pilot; current server-confirmed admin for widget endpoints; viewer and operator principals for share panels", MaxConnections: 3, Health: "/ready", HTTPRoutes: []apicontract.Route{apicontract.ActivityPanel, apicontract.ActivitySettings, apicontract.ActivityConfigure, apicontract.ActivityViewPanel, apicontract.ActivityViewTimeline, apicontract.ActivityViewEmployee, apicontract.ActivityViewEvent, apicontract.ActivityManagedPanels, apicontract.ActivityManagedPanelCreate, apicontract.ActivityManagedPanel, apicontract.ActivityManagedPanelPatch, apicontract.ActivityManagedShareLink, apicontract.ActivityManagedEmployees}, RPCs: []string{"GetPanel", "GetSettings", "Configure", "OperationStatus", "ResolveShare", "CreatePanel", "ListPanels", "GetManagedPanel", "PatchPanel", "RotateShareLink", "ListEmployees", "ViewPanel", "ViewTimeline", "ViewEmployee", "ViewEvent"}},
		{Code: CRMEvents, Version: "v1", MigrationOwner: "crm-events", Dependencies: []string{"gateway", "core-policy"}, Modes: []string{"embedded", "grpc"}, Requirement: "current source and consumer admission", MaxConnections: 5, Concurrency: 2, Health: "/ready", HTTPRoutes: []apicontract.Route{apicontract.ActivityStatus, apicontract.ActivitySync, apicontract.ActivityOperation, apicontract.ActivityEvent}, RPCs: []string{"Apply", "QueryEvents", "GetEvent", "Status", "OperationStatus"}},
		{Code: "gateway", Version: "v1", MigrationOwner: "core", Dependencies: []string{"core-policy", "oauth"}, Modes: []string{"embedded", "grpc"}, Requirement: "one active outbound budget owner; shares Core worker pool; bootstrap account restricted to Core identity", MaxConnections: 6, Health: "/ready", RPCs: []string{"Events", "Users", "Notes", "Tasks", "Pipelines", "CustomFields", "Entities", "CoreBootstrap.GetAccount"}},
	}
	for i := range components {
		if components[i].Code == Activity || components[i].Code == CRMEvents {
			components[i].ProductVersion = "v0"
		}
	}
	return components
}

func Describe(code string) (Component, bool) {
	for _, c := range Components() {
		if c.Code == code {
			return c, true
		}
	}
	return Component{}, false
}
