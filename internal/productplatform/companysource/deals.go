package companysource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/sk1fy/amocrm-pro/internal/companycrm"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

func (c *Client) SendInstallationDeal(ctx context.Context, input companycrm.DealRequest) (companycrm.DealResult, error) {
	var result companycrm.DealResult
	if input.Account != c.config.AccountID || input.Binding != c.config.Revision {
		return result, errors.New("installation deal binding mismatch")
	}
	body, err := json.Marshal(input)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.Origin+companycrm.DealPath, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(raw) > 4096 || resp.StatusCode != http.StatusOK || wire.Decode(raw, &result) != nil {
		return result, errors.New("invalid installation deal response")
	}
	switch result.State {
	case "found":
		base, err := amocrm.AccountBaseURL(result.Host)
		if err == nil && base.Hostname() == result.Host && result.ID > 0 {
			return result, nil
		}
	case "absent":
		if !input.Create && result.ID == 0 && result.Host == "" {
			return result, nil
		}
	case "rejected", "rate_limited":
		if input.Create && result.ID == 0 && result.Host == "" {
			return result, nil
		}
	}
	return companycrm.DealResult{}, errors.New("invalid installation deal outcome")
}

func (c *Client) AddInstallationDealTags(ctx context.Context, input companycrm.DealTagsRequest) error {
	if input.Account != c.config.AccountID || input.Binding != c.config.Revision {
		return errors.New("installation deal binding mismatch")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.Origin+companycrm.DealTagsPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		return errors.New("installation deal tag update failed")
	}
	return nil
}
