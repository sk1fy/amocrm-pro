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
)

const maxAPIResponseBody = 4 << 20

var ErrTransport = errors.New("amoCRM transport failed")

type transportError struct {
	operation string
	cause     error
}

func (e transportError) Error() string        { return e.operation + ": " + e.cause.Error() }
func (e transportError) Unwrap() error        { return e.cause }
func (e transportError) Is(target error) bool { return target == ErrTransport }

type AccessToken struct {
	InstallationID uuid.UUID
	IntegrationID  uuid.UUID
	AccountID      int64
	AccountDomain  string
	Value          string
	TokenVersion   int64
}

type TokenProvider interface {
	Token(context.Context, uuid.UUID) (AccessToken, error)
	RefreshIfCurrent(context.Context, AccessToken) (AccessToken, error)
	MarkReauthRequired(context.Context, uuid.UUID, int64) error
}

type Client struct {
	bootstrapMu       sync.Mutex
	bootstrapAccounts map[string]int64
	httpClient        *http.Client
	tokens            TokenProvider
	limiter           *limiter
	resolveAccount    func(string) (*url.URL, error)
	reauthTimeout     time.Duration
	metrics           *Metrics
}

// NewClient builds a Gateway client on the amoCRM baseline budgets. Use
// NewClientWithLimits to apply per-account overrides from configuration.
func NewClient(httpClient *http.Client, tokens TokenProvider) *Client {
	client, err := NewClientWithLimits(httpClient, tokens, DefaultLimiterConfig())
	if err != nil {
		panic("amoCRM default limiter config is invalid: " + err.Error())
	}
	return client
}

func NewClientWithLimits(httpClient *http.Client, tokens TokenProvider, limits LimiterConfig) (*Client, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	budgets, err := newLimiter(limits)
	if err != nil {
		return nil, err
	}
	return &Client{
		httpClient:     httpClient,
		tokens:         tokens,
		limiter:        budgets,
		resolveAccount: AccountBaseURL,
		reauthTimeout:  2 * time.Second,
	}, nil
}

// WithTokenProvider scopes credentials while retaining this Gateway's one shared
// outbound limiter and metrics. It does not copy the client's mutex.
func (c *Client) WithTokenProvider(tokens TokenProvider) *Client {
	return &Client{httpClient: c.httpClient, tokens: tokens, limiter: c.limiter,
		resolveAccount: c.resolveAccount, reauthTimeout: c.reauthTimeout, metrics: c.metrics}
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

	access, err := c.tokens.Token(ctx, installationID)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.waitBudget(ctx, access); err != nil {
			return fmt.Errorf("wait for amoCRM rate limit: %w", err)
		}
		status, header, response, err := c.request(ctx, access, method, path, requestBody)
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			access, err = c.tokens.RefreshIfCurrent(ctx, access)
			if err != nil {
				return err
			}
			continue
		}
		if status < 200 || status >= 300 {
			if status == http.StatusUnauthorized {
				markContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.reauthTimeout)
				markErr := c.tokens.MarkReauthRequired(markContext, installationID, access.TokenVersion)
				cancel()
				if markErr != nil {
					return fmt.Errorf("mark amoCRM installation reauthorization required: %w", markErr)
				}
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
		c.observeResponse(0, err)
		var urlError *url.Error
		if errors.As(err, &urlError) && urlError.Err != nil {
			err = urlError.Err
		}
		return 0, nil, nil, transportError{operation: "request amoCRM API", cause: err}
	}
	c.observeResponse(response.StatusCode, nil)
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxAPIResponseBody+1))
	if err != nil {
		return 0, nil, nil, transportError{operation: "read amoCRM API response", cause: err}
	}
	if len(contents) > maxAPIResponseBody {
		return 0, nil, nil, errors.New("amoCRM API response body exceeds limit")
	}
	return response.StatusCode, response.Header.Clone(), contents, nil
}
