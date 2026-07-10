package amocrm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

const maxAPIResponseBody = 4 << 20

type AccessToken struct {
	InstallationID uuid.UUID
	IntegrationID  uuid.UUID
	AccountID      int64
	AccountDomain  string
	Value          string
}

type TokenProvider interface {
	Token(context.Context, uuid.UUID, bool) (AccessToken, error)
	MarkReauthRequired(context.Context, uuid.UUID) error
}

type Client struct {
	httpClient     *http.Client
	tokens         TokenProvider
	limiter        *limiter
	resolveAccount func(string) (*url.URL, error)
}

func NewClient(httpClient *http.Client, tokens TokenProvider) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{
		httpClient:     httpClient,
		tokens:         tokens,
		limiter:        newLimiter(rate.Limit(7), 7, rate.Limit(50), 50),
		resolveAccount: AccountBaseURL,
	}
}

func (c *Client) DoJSON(
	ctx context.Context,
	installationID uuid.UUID,
	method string,
	path string,
	requestBody any,
	responseBody any,
) error {
	if !strings.HasPrefix(path, "/api/v4/") && path != "/api/v4/account" {
		return errors.New("amoCRM API path must stay under /api/v4")
	}

	access, err := c.tokens.Token(ctx, installationID, false)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.limiter.wait(ctx, access.IntegrationID, access.AccountID); err != nil {
			return fmt.Errorf("wait for amoCRM rate limit: %w", err)
		}
		status, header, response, err := c.request(ctx, access, method, path, requestBody)
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			access, err = c.tokens.Token(ctx, installationID, true)
			if err != nil {
				return err
			}
			continue
		}
		if status < 200 || status >= 300 {
			if status == http.StatusUnauthorized {
				_ = c.tokens.MarkReauthRequired(context.WithoutCancel(ctx), installationID)
			}
			return classifyResponse(status, header, time.Now())
		}
		if responseBody != nil && len(response) > 0 {
			if err := json.Unmarshal(response, responseBody); err != nil {
				return fmt.Errorf("decode amoCRM API response: %w", err)
			}
		}
		return nil
	}
	return errors.New("unreachable amoCRM retry state")
}

func (c *Client) request(
	ctx context.Context,
	access AccessToken,
	method string,
	path string,
	requestBody any,
) (int, http.Header, []byte, error) {
	baseURL, err := c.resolveAccount(access.AccountDomain)
	if err != nil {
		return 0, nil, nil, err
	}
	reference, err := url.Parse(path)
	if err != nil || reference.IsAbs() || reference.Host != "" {
		return 0, nil, nil, errors.New("invalid relative amoCRM API path")
	}
	endpoint := baseURL.ResolveReference(reference)

	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("encode amoCRM API request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("create amoCRM API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+access.Value)
	request.Header.Set("Accept", "application/json")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("request amoCRM API: %w", err)
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxAPIResponseBody+1))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read amoCRM API response: %w", err)
	}
	if len(contents) > maxAPIResponseBody {
		return 0, nil, nil, errors.New("amoCRM API response body exceeds limit")
	}
	return response.StatusCode, response.Header.Clone(), contents, nil
}

type limiter struct {
	mu               sync.Mutex
	integrationRate  rate.Limit
	integrationBurst int
	accountRate      rate.Limit
	accountBurst     int
	integrations     map[uuid.UUID]*rate.Limiter
	accounts         map[int64]*rate.Limiter
}

func newLimiter(integrationRate rate.Limit, integrationBurst int, accountRate rate.Limit, accountBurst int) *limiter {
	return &limiter{
		integrationRate:  integrationRate,
		integrationBurst: integrationBurst,
		accountRate:      accountRate,
		accountBurst:     accountBurst,
		integrations:     make(map[uuid.UUID]*rate.Limiter),
		accounts:         make(map[int64]*rate.Limiter),
	}
}

func (l *limiter) wait(ctx context.Context, integrationID uuid.UUID, accountID int64) error {
	l.mu.Lock()
	integrationLimiter, ok := l.integrations[integrationID]
	if !ok {
		integrationLimiter = rate.NewLimiter(l.integrationRate, l.integrationBurst)
		l.integrations[integrationID] = integrationLimiter
	}
	accountLimiter, ok := l.accounts[accountID]
	if !ok {
		accountLimiter = rate.NewLimiter(l.accountRate, l.accountBurst)
		l.accounts[accountID] = accountLimiter
	}
	l.mu.Unlock()
	if err := integrationLimiter.Wait(ctx); err != nil {
		return err
	}
	return accountLimiter.Wait(ctx)
}
