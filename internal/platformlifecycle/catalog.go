package platformlifecycle

import "github.com/google/uuid"

const CatalogPath = "/internal/platform/v1/widget-catalog"

type IntegrationRegistration struct {
	ID     uuid.UUID  `json:"integration_id"`
	After  *uuid.UUID `json:"after,omitempty"`
	Active bool       `json:"active"`
}

type CatalogPolicy struct {
	Confirmed []ConfirmedInstallation `json:"confirmed,omitempty"`
	Product   string                  `json:"product_code"`
}

type ConfirmedInstallation struct {
	ID         uuid.UUID `json:"id"`
	Generation int64     `json:"generation"`
}

const ConfirmationPageSize = 64
