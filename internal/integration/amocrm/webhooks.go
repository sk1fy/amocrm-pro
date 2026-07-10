package amocrm

import (
	"context"
	"net/http"
	"net/url"

	"github.com/google/uuid"
)

type Webhook struct {
	ID          int64    `json:"id"`
	Destination string   `json:"destination"`
	Disabled    bool     `json:"disabled"`
	Settings    []string `json:"settings"`
}

type webhookList struct {
	Embedded struct {
		Webhooks []Webhook `json:"webhooks"`
	} `json:"_embedded"`
}

type WebhookSpec struct {
	Destination string   `json:"destination"`
	Settings    []string `json:"settings"`
}

func (c *Client) ListWebhooks(ctx context.Context, installationID uuid.UUID, destination string) ([]Webhook, error) {
	query := url.Values{}
	if destination != "" {
		query.Set("filter[destination]", destination)
	}
	path := "/api/v4/webhooks"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var response webhookList
	if err := c.DoJSON(ctx, installationID, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.Embedded.Webhooks, nil
}

func (c *Client) RegisterWebhook(ctx context.Context, installationID uuid.UUID, spec WebhookSpec) (Webhook, error) {
	var response Webhook
	if err := c.DoJSON(ctx, installationID, http.MethodPost, "/api/v4/webhooks", spec, &response); err != nil {
		return Webhook{}, err
	}
	return response, nil
}

func (c *Client) DeleteWebhook(ctx context.Context, installationID uuid.UUID, destination string) error {
	return c.DoJSON(ctx, installationID, http.MethodDelete, "/api/v4/webhooks", map[string]string{
		"destination": destination,
	}, nil)
}
