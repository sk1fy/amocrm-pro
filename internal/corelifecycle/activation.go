package corelifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

var ErrActivationUnavailable = errors.New("activation unavailable for this installation")

type ActivationClient struct {
	endpoint string
	client   *http.Client
}

func NewActivationClient(origin, identityDir string) (*ActivationClient, error) {
	endpoint, client, err := platformClient(origin, identityDir, wire.ActivationPath)
	if err != nil {
		return nil, err
	}
	return &ActivationClient{endpoint: endpoint.String(), client: client}, nil
}

func (c *ActivationClient) Close() { c.client.CloseIdleConnections() }

func (c *ActivationClient) Apply(ctx context.Context, input wire.ActivationRequest) (wire.ActivationResult, error) {
	var result wire.ActivationResult
	body, err := json.Marshal(input)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
		if err != nil || len(raw) > 4096 || wire.Decode(raw, &result) != nil {
			return result, errors.New("invalid activation response")
		}
		return result, nil
	case http.StatusNotFound:
		return result, ErrActivationUnavailable
	default:
		return result, errors.New("Platform activation unavailable")
	}
}
