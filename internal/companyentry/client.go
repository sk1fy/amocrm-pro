package companyentry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

type UpstreamError struct{ Status int }

func (e *UpstreamError) Error() string { return "company authority request failed" }

type Client struct {
	BaseURL, Token string
	HTTP           *http.Client
}

func (c *Client) request(ctx context.Context, path string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Service "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(req)
	if err != nil {
		return errors.New("company authority unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return &UpstreamError{response.StatusCode}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16385))
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid company authority response")
	}
	return nil
}
func (c *Client) Status(ctx context.Context, account string) (Status, error) {
	var result Status
	err := c.request(ctx, "/api/v1/provisioning/amocrm/company-status", map[string]string{"accountId": account}, &result)
	if err == nil && (result.State != "absent" && result.State != "present" && result.State != "blocked" || result.State == "present" && result.CompanyID == [16]byte{} || result.State == "absent" && result.CompanyID != [16]byte{}) {
		err = errors.New("invalid company status")
	}
	return result, err
}
func (c *Client) Enter(ctx context.Context, in EntryInput) (Entry, error) {
	var result Entry
	err := c.request(ctx, "/api/v1/provisioning/amocrm/widget-entries", in, &result)
	return result, err
}
